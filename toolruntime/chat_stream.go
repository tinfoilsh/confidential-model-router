package toolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// chatStreamer owns one logical chat.completion.chunk stream across
// every tool iteration, keeping id/created/model pinned to the first
// upstream chunk's values.
type chatStreamer struct {
	streamBase

	clientRequestedUsage bool
	// eventFlags tracks which tinfoil-event marker families the
	// caller opted into via the `X-Tinfoil-Events` header.
	eventFlags tinfoilEventFlags
	emitter    *citationEmitter

	// roleEmitted defers the initial assistant role-delta chunk until
	// the first upstream frame, so it can carry upstream's identity.
	roleEmitted bool

	// id and created are captured from the first upstream chunk we see
	// and pinned for the rest of the logical stream. Subsequent
	// upstream turns keep re-sending fresh values; we ignore them so
	// the client sees one coherent chat.completion.
	id             string
	created        int64
	identityPinned bool
}

// chatIterationResult summarizes one upstream stream's payload once the
// stream terminated: accumulated content, any parsed tool calls, and the
// finish reason the upstream reported. Callers use this to decide whether
// to loop (router-owned tool calls present) or finalize (no tool calls or
// only client-owned tool calls remain).
type chatIterationResult struct {
	content      string
	reasoning    string
	toolCalls    []toolCall
	rawToolCalls []any
	finishReason string
	usage        map[string]any
}

// chatToolCallBuilder assembles OpenAI streaming tool_call deltas into the
// final tool_call objects. OpenAI emits each function's name and id on
// the first delta for a given index, and the arguments JSON in
// incremental string fragments.
type chatToolCallBuilder struct {
	entries []*chatToolCallEntry
}

type chatToolCallEntry struct {
	index        int
	id           string
	toolType     string
	functionName string
	arguments    []byte
}

// ---------------------------------------------------------------------------
// chatIterationResult methods
// ---------------------------------------------------------------------------

// assistantMessage builds the OpenAI-shaped assistant message that the
// router appends to the messages array between iterations so the next
// upstream call sees the tool_calls the model just emitted. We preserve
// the exact raw tool_calls from the stream so serialization round-trips
// byte-for-byte; the parsed form is only used for loop control.
func (r *chatIterationResult) assistantMessage() map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": r.content,
	}
	if len(r.rawToolCalls) > 0 {
		message["tool_calls"] = r.rawToolCalls
	}
	return message
}

// ---------------------------------------------------------------------------
// chatStreamer methods
// ---------------------------------------------------------------------------

// runIteration posts one upstream streaming call and drives the SSE pump
// until the upstream signals done. Content deltas flow through the citation
// emitter and out to the client immediately. Tool-call deltas accumulate
// without being forwarded to the client until we know whether they are
// router-owned or client-owned.
func (s *chatStreamer) runIteration(
	ctx context.Context,
	em *manager.EnclaveManager,
	modelName string,
	reqBody map[string]any,
	requestHeaders http.Header,
) (chatIterationResult, error) {
	body, err := s.openUpstreamSSE(ctx, em, modelName, "/v1/chat/completions", reqBody, requestHeaders)
	if err != nil {
		return chatIterationResult{}, err
	}
	defer body.Close()
	return s.pumpUpstream(newSSEReader(body))
}

// pumpUpstream reads every SSE frame from upstream for the current
// iteration, forwards assistant content (through the citation emitter) and
// finish metadata to the client, and accumulates tool-call deltas plus
// usage for loop control. It returns when the upstream sends `[DONE]` or
// otherwise ends the SSE stream. A bare EOF before `[DONE]` is treated as
// an abrupt upstream disconnect and surfaced to the caller as an error so
// the client sees a terminating error frame rather than a silently
// truncated completion.
func (s *chatStreamer) pumpUpstream(reader *sseReader) (chatIterationResult, error) {
	builder := &chatToolCallBuilder{}
	result := chatIterationResult{}
	doneSeen := false
	for {
		if s.writeErr != nil {
			return result, s.writeErr
		}
		frame, err := reader.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, err
		}
		if frame.data == "[DONE]" {
			doneSeen = true
			break
		}
		if frame.data == "" {
			continue
		}

		var chunk map[string]any
		if jsonErr := json.Unmarshal([]byte(frame.data), &chunk); jsonErr != nil {
			return result, newUpstreamStreamError("upstream emitted malformed SSE JSON: " + jsonErr.Error())
		}
		// Standalone mid-stream error object (e.g. context-length
		// exceeded): surface it as a terminating error.
		if errObj, ok := chunk["error"].(map[string]any); ok {
			return result, newUpstreamJSONError(errObj)
		}
		s.ensureStreamIdentity(chunk)
		// The assistant role-delta chunk is deferred until we have read
		// one upstream chunk so it can carry upstream's id/created/model
		// instead of router-synthesized placeholders. We still fire it
		// exactly once across the whole logical stream.
		s.ensureRoleEmitted()

		if usage, ok := chunk["usage"].(map[string]any); ok {
			result.usage = usage
		}

		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}
		if reason := stringValue(choice["finish_reason"]); reason != "" {
			result.finishReason = reason
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if content := stringValue(delta["content"]); content != "" {
			result.content += content
			s.emitContentDelta(content)
		}
		// Reasoning deltas (reasoning_content / reasoning) never carry
		// citations so they bypass the citation emitter. They must still
		// flow through to the client as-is so reasoning models keep
		// driving the live thinking UI when web search is enabled.
		if r := stringValue(delta["reasoning_content"]); r != "" {
			result.reasoning += r
		} else if r := stringValue(delta["reasoning"]); r != "" {
			result.reasoning += r
		}
		s.emitReasoningDelta(delta)
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			builder.ingest(rawCalls)
		}
	}

	result.toolCalls = builder.toolCalls()
	result.rawToolCalls = builder.raw()
	if result.usage != nil {
		s.usageTotals.Add(&upstreamJSONResponse{body: map[string]any{"usage": result.usage}})
	}
	if !doneSeen {
		return result, newUpstreamStreamError("upstream stream ended without a terminal [DONE] marker")
	}
	return result, nil
}

// ensureStreamIdentity pins the id/created/model to the first upstream
// chunk's values exactly once and ignores every subsequent chunk so the
// client sees one coherent chat completion across an arbitrary number of
// internal tool iterations. Each field is captured independently because
// some upstreams emit id/created on the very first frame and model on a
// later frame (or vice-versa). identityPinned flips to true once ALL
// three have been populated, after which further upstream values are
// simply ignored.
func (s *chatStreamer) ensureStreamIdentity(chunk map[string]any) {
	if s.identityPinned {
		return
	}
	if s.id == "" {
		if id := stringValue(chunk["id"]); id != "" {
			s.id = id
		}
	}
	if s.created == 0 {
		if created := int64(numberValue(chunk["created"])); created > 0 {
			s.created = created
		}
	}
	if s.model == "" {
		if model := stringValue(chunk["model"]); model != "" {
			s.model = model
		}
	}
	if s.id != "" && s.created != 0 && s.model != "" {
		s.identityPinned = true
	}
}

// streamID returns the stable id for the current logical chat completion,
// falling back to a router-minted one if upstream never sent one by the
// time we need to emit a chunk. The fallback is remembered so subsequent
// chunks keep the same id. Every OpenAI-compatible inference server
// (vLLM, TGI, llama.cpp) emits `id` on the very first chunk; hitting
// this fallback indicates an upstream regression and is logged once per
// affected stream so operators can spot fleet drift.
func (s *chatStreamer) streamID() string {
	if s.id == "" {
		s.id = "chatcmpl-" + uuid.NewString()
		log.Printf("toolruntime: upstream omitted chat.completion.chunk id, router minted fallback %s model=%s", s.id, s.model)
	}
	return s.id
}

// streamCreated returns the stable creation timestamp for the current
// logical chat completion, falling back to the router's wall clock if
// upstream never sent one by the time we need to emit a chunk. A missing
// `created` field is an OpenAI-spec violation from upstream; we heal the
// stream rather than fail it but log once per affected stream.
func (s *chatStreamer) streamCreated() int64 {
	if s.created == 0 {
		s.created = time.Now().Unix()
		log.Printf("toolruntime: upstream omitted chat.completion.chunk created, router stamped fallback %d model=%s", s.created, s.model)
	}
	return s.created
}

// streamModel returns the stable model name for the current logical
// chat completion. Every OpenAI-compatible inference server echoes
// request.model on its streaming chunks; a missing value indicates
// an upstream bug and is surfaced as a loud stream error via
// streamBase.validateStreamModel so we do not mask a fleet regression
// behind a cached config label.
func (s *chatStreamer) streamModel() string {
	return s.validateStreamModel("chunk.model")
}

// ensureRoleEmitted writes the initial assistant role-delta chunk exactly
// once, deferred until the first upstream frame has been observed so the
// chunk carries the upstream-provided id/created/model instead of a
// router-synthesized placeholder.
func (s *chatStreamer) ensureRoleEmitted() {
	if s.roleEmitted {
		return
	}
	s.roleEmitted = true
	s.writeChunk(map[string]any{
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant"},
			},
		},
	})
}

// emitContentDelta feeds the incoming content fragment through the citation
// emitter and writes whatever the emitter releases to the client as a
// content delta plus any annotation chunks for links that just resolved.
func (s *chatStreamer) emitContentDelta(fragment string) {
	contentChunk, annotations := s.emitter.push(fragment)
	if contentChunk != "" {
		s.writeChunk(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"content": contentChunk},
				},
			},
		})
	}
	if len(annotations) > 0 {
		s.writeChunk(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"annotations": nestedAnnotationsFromMatches(annotations)},
				},
			},
		})
	}
}

// emitReasoningDelta forwards upstream `reasoning_content` and `reasoning`
// delta fields to the client unmodified so reasoning models keep
// streaming their thinking tokens when the web-search tool loop is
// active. Both keys can appear on the same delta; preserve whichever the
// upstream sent so clients that only recognize one of the two still see
// something.
func (s *chatStreamer) emitReasoningDelta(delta map[string]any) {
	if delta == nil {
		return
	}
	reasoningDelta := map[string]any{}
	if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
		reasoningDelta["reasoning_content"] = reasoning
	}
	if reasoning := stringValue(delta["reasoning"]); reasoning != "" {
		reasoningDelta["reasoning"] = reasoning
	}
	if len(reasoningDelta) == 0 {
		return
	}
	s.writeChunk(map[string]any{
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": reasoningDelta,
			},
		},
	})
}

// flushCitations drains any trailing content still held back by the
// emitter at end-of-stream so the user sees every rune the model produced.
func (s *chatStreamer) flushCitations() {
	contentChunk, annotations := s.emitter.flush()
	if contentChunk != "" {
		s.writeChunk(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"content": contentChunk},
				},
			},
		})
	}
	if len(annotations) > 0 {
		s.writeChunk(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"annotations": nestedAnnotationsFromMatches(annotations)},
				},
			},
		})
	}
}

// executeTool runs a single MCP tool call and, when the caller opted
// into the router-owned marker stream via `X-Tinfoil-Events: web_search`,
// emits `<tinfoil-event>` progress markers inside the assistant content
// delta stream. Markers are spec-invisible to OpenAI SDKs that do not
// opt in: because the payload rides inside a regular chat.completion.chunk
// `delta.content` string, strict decoders simply render them as text.
// Opt-in clients strip them with a single regex before rendering.
// Failures are still returned to the caller so the tool output carries
// the raw error text (matching the non-streaming path that serializes
// err.Error() into the tool-result message).
func (s *chatStreamer) executeTool(ctx context.Context, registry *sessionRegistry, call toolCall) (string, error) {
	return executeToolWithProgress(ctx, registry, s.citations, &chatToolProgressEmitter{streamer: s}, call)
}

// emitTinfoilEventMarker writes a single `<tinfoil-event>` marker as the
// `delta.content` of a chat.completion.chunk frame. When the caller did
// not opt into the marker stream, this is a no-op so strict SDKs see a
// pristine spec-conformant stream.
func (s *chatStreamer) emitTinfoilEventMarker(id, status string, action map[string]any, reason string, sources []toolCallSource) {
	if !s.eventFlags.webSearch {
		return
	}
	marker := tinfoilEventMarker(id, status, action, reason, sources)
	s.writeChunk(map[string]any{
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{"content": marker},
			},
		},
	})
}

// writeChunk serializes a single chat.completion.chunk to the client,
// filling in the stable id/created/model fields captured from upstream.
// Accessors lazily fall back to router-minted defaults if upstream never
// sent a given field by the time the chunk is emitted; in practice every
// compliant SSE producer sends them on the very first frame.
func (s *chatStreamer) writeChunk(partial map[string]any) {
	chunk := map[string]any{
		"id":      s.streamID(),
		"object":  "chat.completion.chunk",
		"created": s.streamCreated(),
		"model":   s.streamModel(),
	}
	for key, value := range partial {
		chunk[key] = value
	}
	s.emitData(chunk)
}

// emitData writes one `data:` SSE frame to the client and flushes.
// Latches the first write error into s.writeErr; see streamBase.writeErr.
func (s *chatStreamer) emitData(body map[string]any) {
	if s.writeErr != nil {
		return
	}
	if err := sseData(s.w, body); err != nil {
		s.writeErr = err
		return
	}
	s.flusher.Flush()
}

// finalize emits any remaining buffered citations, forwards client-owned
// tool_calls the model asked the user to execute, writes the finish-reason
// chunk and final usage chunk, fires the billing event, and closes the
// stream with `[DONE]`.
func (s *chatStreamer) finalize(
	r *http.Request,
	em *manager.EnclaveManager,
	modelName string,
	result chatIterationResult,
	clientToolCalls []toolCall,
) error {
	// Defensive: in the degenerate case where upstream never emitted a
	// single chunk before signaling done (and the pump therefore never
	// ran ensureRoleEmitted), the finish chunk below would be the
	// client's first visible delta; still emit role first to keep the
	// protocol contract.
	s.ensureRoleEmitted()
	s.flushCitations()

	if len(clientToolCalls) > 0 {
		raw := rawToolCallsFromParsed(clientToolCalls, result.rawToolCalls)
		s.writeChunk(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": raw},
				},
			},
		})
	}

	finishReason := result.finishReason
	if len(clientToolCalls) > 0 {
		finishReason = "tool_calls"
	} else if finishReason == "" {
		finishReason = "stop"
	}
	s.writeChunk(map[string]any{
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
	})

	finalUsage := s.finalUsage()
	if s.clientRequestedUsage && finalUsage != nil {
		s.writeChunk(map[string]any{
			"choices": []any{},
			"usage":   finalUsage,
		})
	}

	if s.writeErr == nil {
		if _, err := io.WriteString(s.w, "data: [DONE]\n\n"); err != nil {
			s.writeErr = err
		} else {
			s.flusher.Flush()
		}
	}

	if s.usageMetricsRequested && finalUsage != nil {
		s.w.Header().Set(manager.UsageMetricsResponseHeader, formatUsageHeader(usageFromRaw(finalUsage)))
	}
	s.emitBillingEvent(r, em, modelName, finalUsage)
	return s.writeErr
}

// finalUsage assembles the aggregated usage block across every upstream
// iteration in the OpenAI Chat Completions shape. The underlying
// accumulator already handles turns where upstream omitted the usage block
// entirely by falling back to zero.
func (s *chatStreamer) finalUsage() map[string]any {
	usage := s.usageTotals.Usage()
	if usage == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
	}
}

// terminateWithError is the single point at which a mid-stream upstream
// failure is surfaced to the client. If no SSE has started yet we return
// the upstream error to the caller so Handle turns it into a plain JSON
// error response; otherwise we emit a top-level error data frame in the
// shape OpenAI SDKs recognize mid-stream, followed by `[DONE]` so SDKs can
// close their streams cleanly.
func (s *chatStreamer) terminateWithError(r *http.Request, em *manager.EnclaveManager, modelName string, err error) error {
	if !s.headersWritten {
		return err
	}
	s.flushCitations()
	s.emitData(map[string]any{"error": upstreamErrorPayload(err)})
	if s.writeErr == nil {
		if _, werr := io.WriteString(s.w, "data: [DONE]\n\n"); werr != nil {
			s.writeErr = werr
		} else {
			s.flusher.Flush()
		}
	}
	s.emitBillingEvent(r, em, modelName, s.finalUsage())
	return nil
}

// ---------------------------------------------------------------------------
// chatToolCallBuilder methods
// ---------------------------------------------------------------------------

// ingest merges one tool_call delta list into the builder's running state.
// It preserves the original wire-format fragment in s.raw so the assistant
// message emitted on the next iteration carries bytewise-identical tool
// calls to whatever upstream produced.
func (b *chatToolCallBuilder) ingest(rawCalls []any) {
	for _, item := range rawCalls {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		idx := int(numberValue(m["index"]))
		if idx < 0 {
			// OpenAI's streaming tool_call protocol uses a 0-based
			// index; a negative value is either a protocol bug or
			// a hostile/malformed upstream frame. Dropping the
			// delta is safer than indexing into b.entries with a
			// negative int, which would panic the streamer.
			continue
		}
		for len(b.entries) <= idx {
			b.entries = append(b.entries, &chatToolCallEntry{index: len(b.entries)})
		}
		entry := b.entries[idx]
		if id := stringValue(m["id"]); id != "" {
			entry.id = id
		}
		if t := stringValue(m["type"]); t != "" {
			entry.toolType = t
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if name := stringValue(fn["name"]); name != "" {
				entry.functionName = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				entry.arguments = append(entry.arguments, args...)
			}
		}
	}
}

// toolCalls returns the assembled tool_call objects as the parsed router
// type. Arguments are JSON-decoded when possible; malformed arguments are
// preserved as an empty map so the loop can still surface the tool call
// rather than silently drop it.
func (b *chatToolCallBuilder) toolCalls() []toolCall {
	out := make([]toolCall, 0, len(b.entries))
	for _, entry := range b.entries {
		if entry == nil || entry.functionName == "" {
			continue
		}
		args := map[string]any{}
		if len(entry.arguments) > 0 {
			raw, _ := sanitizeToolCallArgumentsJSON(entry.arguments)
			_ = json.Unmarshal(raw, &args)
		}
		out = append(out, toolCall{
			id:        entry.id,
			name:      entry.functionName,
			arguments: args,
		})
	}
	return out
}

// raw returns the tool_calls array in the wire shape OpenAI uses for
// non-streaming completions. Used to rehydrate the assistant message we
// prepend to the next iteration's messages array.
func (b *chatToolCallBuilder) raw() []any {
	calls := make([]any, 0, len(b.entries))
	for _, entry := range b.entries {
		if entry == nil || entry.functionName == "" {
			continue
		}
		toolType := entry.toolType
		if toolType == "" {
			toolType = "function"
		}
		calls = append(calls, map[string]any{
			"id":   entry.id,
			"type": toolType,
			"function": map[string]any{
				"name":      entry.functionName,
				"arguments": string(entry.arguments),
			},
		})
	}
	return calls
}

// ---------------------------------------------------------------------------
// Free functions
// ---------------------------------------------------------------------------

// runChatStreaming is the streaming entry point for the Chat Completions
// route. It drives the same tool loop as runChatLoop but consumes the
// upstream model's stream incrementally so that assistant content deltas
// and citation annotations are forwarded to the client live rather than
// synthesized at the end of the turn. When the client opted into the
// router-owned `X-Tinfoil-Events: web_search` marker stream, router-owned
// web_search tool-call progress is surfaced inline via
// `<tinfoil-event>...</tinfoil-event>` markers carried inside the normal
// `delta.content` text. Clients that did not opt in see a pristine
// spec-conformant chat.completion.chunk stream with no markers.
//
// The function assumes isStream(body) is already true. Non-streaming callers
// continue to go through runChatLoop so existing behavior is preserved for
// SDKs that do not want SSE back.
func runChatStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	em *manager.EnclaveManager,
	registry *sessionRegistry,
	body map[string]any,
	modelName string,
	requestHeaders http.Header,
	prompt *mcp.GetPromptResult,
	dl *devLog,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	tid := debugTraceID()
	searchOpts := parseChatWebSearchOptions(body)
	tools := registry.allTools()
	ownedTools := registry.ownedTools()
	toolSchemas := schemaLookup(tools)
	reqBody := cloneJSONMap(body)
	delete(reqBody, "web_search_options")
	delete(reqBody, "filters")
	delete(reqBody, "pii_check_options")
	delete(reqBody, "prompt_injection_check_options")
	stripRouterOwnedIncludes(reqBody)
	reqBody["stream"] = true
	reqBody["stream_options"] = map[string]any{"include_usage": true}
	applyParallelToolCallsPolicy(reqBody)
	reqBody["tools"] = append(existingTools(reqBody["tools"]), chatTools(tools)...)
	reqBody["messages"] = prependChatPrompt(prompt, reqBody["messages"])

	usageMetricsRequested := r.Header.Get(manager.UsageMetricsRequestHeader) == "true"
	clientRequestedUsage := r.Header.Get("X-Tinfoil-Client-Requested-Usage") == "true"
	eventFlags := parseTinfoilEventFlags(r.Header)

	if debugEnabled {
		ownedNames := make([]string, 0, len(ownedTools))
		for n := range ownedTools {
			ownedNames = append(ownedNames, n)
		}
		debugLogf("toolruntime:%s chatstream.start model=%s search_ctx=%q profiles=%v ownedTools=%v schemas=%d",
			tid, modelName, searchOpts.searchContextSize, registry.profileNames(), ownedNames, len(toolSchemas))
	}

	streamer := &chatStreamer{
		streamBase: streamBase{
			w:                     w,
			flusher:               flusher,
			usageMetricsRequested: usageMetricsRequested,
			citations:             &citationState{nextIndex: 1},
			toolCalls:             &toolCallLog{},
			usageTotals:           &usageAccumulator{},
		},
		clientRequestedUsage: clientRequestedUsage,
		eventFlags:           eventFlags,
		emitter:              nil,
	}
	streamer.emitter = newCitationEmitter(streamer.citations)

	for i := 0; i < maxToolIterations; i++ {
		if msgs, ok := reqBody["messages"].([]any); ok {
			if n := sanitizeAssistantToolCallsInMessages(msgs); n > 0 {
				debugLogf("toolruntime:%s chatstream.iter=%d history_sanitized count=%d", tid, i, n)
			}
		}
		if debugEnabled {
			msgs, _ := reqBody["messages"].([]any)
			debugLogf("toolruntime:%s chatstream.iter=%d upstream.request messages_n=%d history=%s", tid, i, len(msgs), debugMessagesSummary(msgs, 200))
		}
		iterStart := time.Now()
		result, err := streamer.runIteration(ctx, em, modelName, reqBody, requestHeaders)
		if err != nil {
			debugLogf("toolruntime:%s chatstream.iter=%d upstream.error elapsed=%s err=%v", tid, i, time.Since(iterStart), err)
			return streamer.terminateWithError(r, em, modelName, err)
		}
		// If the client has disconnected, stop before spending another
		// round of MCP tool calls and upstream tokens on work nobody is
		// listening for.
		if streamer.writeErr != nil {
			debugLogf("toolruntime:%s chatstream.client_disconnected at iter=%d", tid, i)
			streamer.emitBillingEvent(r, em, modelName, streamer.finalUsage())
			return nil
		}

		// Dev log: turn header + tokens + thinking + content
		dl.WriteTurnHeader(i + 1)
		dl.WriteTokens(result.usage)
		dl.WriteStreamedThinkingAndContent(result.reasoning, result.content)

		// Mid-turn client tool_calls are intentionally dropped; only the
		// final turn (no router tool calls to resolve) forwards them to
		// the client. This preserves parity with runChatLoop.
		routerToolCalls, clientToolCalls := splitToolCalls(ownedTools, result.toolCalls)
		if debugEnabled {
			toolNames := make([]string, 0, len(result.toolCalls))
			for _, c := range result.toolCalls {
				toolNames = append(toolNames, c.name)
			}
			debugLogf("toolruntime:%s chatstream.iter=%d upstream.response elapsed=%s finish=%q total_tool_calls=%d router_tool_calls=%d client_tool_calls=%d tool_names=%v content=%q",
				tid, i, time.Since(iterStart), result.finishReason, len(result.toolCalls), len(routerToolCalls), len(clientToolCalls), toolNames, debugPreview(result.content, 240))
		}
		if len(routerToolCalls) == 0 {
			debugLogf("toolruntime:%s chatstream.done iterations=%d (no router tool calls)", tid, i+1)
			dl.WriteFinish(result.finishReason)
			return streamer.finalize(r, em, modelName, result, clientToolCalls)
		}

		mixedTurn := len(clientToolCalls) > 0
		dl.WriteToolCalls(routerToolCalls)
		tracePhase := fmt.Sprintf("chatstream.iter=%d", i)
		var messages []any
		if !mixedTurn {
			messages, _ = reqBody["messages"].([]any)
			messages = append(messages, result.assistantMessage())
		}
		for _, call := range routerToolCalls {
			tstart := time.Now()
			output := resolveStreamingRouterToolCall(
				ctx, call, searchOpts, toolSchemas, streamer.citations, streamer.toolCalls,
				func(ctx context.Context, call toolCall) (string, error) {
					return streamer.executeTool(ctx, registry, call)
				},
				tracePhase, tid,
			)
			dl.WriteToolExec(call.name, call.arguments, output, time.Since(tstart), "")
			if !mixedTurn {
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": call.id,
					"content":      output,
				})
			}
		}
		// Mixed turn: see the mixed-turn contract on runToolLoop.
		if mixedTurn {
			debugLogf("toolruntime:%s chatstream.mixed_turn iterations=%d (router+client calls in one turn; finalizing)", tid, i+1)
			return streamer.finalize(r, em, modelName, result, clientToolCalls)
		}
		reqBody["messages"] = messages
	}
	debugLogf("toolruntime:%s chatstream.exhausted iterations=%d forcing final answer", tid, maxToolIterations)

	// Exhausted the tool budget; force a final-answer turn with tools removed
	// and stream it to the client directly.
	finalBody := forcedFinalChatRequest(reqBody)
	finalBody["stream"] = true
	finalBody["stream_options"] = map[string]any{"include_usage": true}
	result, err := streamer.runIteration(ctx, em, modelName, finalBody, requestHeaders)
	if err != nil {
		return streamer.terminateWithError(r, em, modelName, err)
	}
	return streamer.finalize(r, em, modelName, result, nil)
}

// rawToolCallsFromParsed filters the streamed raw tool_calls down to the
// client-owned ones and re-indexes them starting at zero so the final
// stream chunk emits a delta.tool_calls array in the shape clients expect:
// contiguous `index` values identifying positions in the client-visible
// tool_calls list. The `id`, `type`, and `function` fields are preserved
// byte-for-byte from what upstream produced.
func rawToolCallsFromParsed(parsed []toolCall, raw []any) []any {
	filtered := make([]any, 0, len(parsed))
	next := 0
	for _, item := range raw {
		source, _ := item.(map[string]any)
		if source == nil || next >= len(parsed) || !rawToolCallMatchesParsed(source, parsed[next]) {
			continue
		}
		reindexed := map[string]any{
			"index":    len(filtered),
			"id":       source["id"],
			"type":     source["type"],
			"function": source["function"],
		}
		filtered = append(filtered, reindexed)
		next++
	}
	return filtered
}

func rawToolCallMatchesParsed(source map[string]any, call toolCall) bool {
	if source == nil {
		return false
	}
	function, _ := source["function"].(map[string]any)
	if function == nil || stringValue(function["name"]) != call.name {
		return false
	}
	if call.id != "" && stringValue(source["id"]) != call.id {
		return false
	}
	return true
}

// nestedAnnotationsFromMatches converts the internal citationAnnotation
// slice into the nested url_citation shape documented by OpenAI for Chat
// Completions responses so SDKs and UIs that already consume the final
// non-streaming annotation format keep working unchanged.
func nestedAnnotationsFromMatches(matches []citationAnnotation) []any {
	out := make([]any, 0, len(matches))
	for _, match := range matches {
		citation := map[string]any{
			"url":         match.source.url,
			"start_index": match.startIndex,
			"end_index":   match.endIndex,
		}
		if match.source.title != "" {
			citation["title"] = match.source.title
		}
		out = append(out, map[string]any{
			"type":         "url_citation",
			"url_citation": citation,
		})
	}
	return out
}
