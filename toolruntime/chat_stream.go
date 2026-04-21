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
	reqBody["parallel_tool_calls"] = false
	reqBody["tools"] = append(existingTools(reqBody["tools"]), chatTools(tools)...)
	reqBody["messages"] = prependChatPrompt(prompt, reqBody["messages"])

	usageMetricsRequested := r.Header.Get(manager.UsageMetricsRequestHeader) == "true"
	clientRequestedUsage := r.Header.Get("X-Tinfoil-Client-Requested-Usage") == "true"
	eventsEnabled := tinfoilEventsEnabled(r.Header)

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
			usageTotals:           &usageAccumulator{},
		},
		clientRequestedUsage: clientRequestedUsage,
		eventsEnabled:        eventsEnabled,
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
			return streamer.finalize(r, em, modelName, result, clientToolCalls)
		}

		messages, _ := reqBody["messages"].([]any)
		messages = append(messages, result.assistantMessage())
		for _, call := range routerToolCalls {
			applyWebSearchOptionsToToolCall(call.name, call.arguments, searchOpts)
			sanitizeToolCallArguments(call.name, call.arguments)
			coerceArgumentsToSchema(call.name, call.arguments, toolSchemas)
			debugLogf("toolruntime:%s chatstream.iter=%d tool.call name=%s args=%s", tid, i, call.name, debugPreview(call.arguments, 400))
			tstart := time.Now()
			output, toolErr := streamer.executeTool(ctx, registry, call)
			record := toolCallRecord{
				name:      call.name,
				arguments: call.arguments,
			}
			if toolErr != nil {
				debugLogf("toolruntime:%s chatstream.iter=%d tool.error name=%s elapsed=%s err=%v", tid, i, call.name, time.Since(tstart), toolErr)
				output = humanizeToolArgError(call.name, toolErr, call.arguments)
				record.errorReason = publicToolErrorReason(call.name, toolErr)
			} else {
				record.resultURLs = extractToolOutputURLs(output)
				debugLogf("toolruntime:%s chatstream.iter=%d tool.result name=%s elapsed=%s output_len=%d urls=%v preview=%q",
					tid, i, call.name, time.Since(tstart), len(output), record.resultURLs, debugPreview(output, 400))
			}
			streamer.citations.recordToolCall(record)
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": call.id,
				"content":      output,
			})
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

// chatStreamer owns the single logical chat.completion.chunk stream that
// goes back to the client across however many tool iterations the model
// needs. It keeps id/created/model pinned to the values the first upstream
// chunk reports so downstream SDKs see one continuous logical response.
type chatStreamer struct {
	// streamBase holds shared state (client writer, flusher, citations,
	// usage totals, headers-written flag, upstream headers, pinned
	// model name, latched writeErr) common to every SSE streamer. See
	// stream_base.go for the full documentation.
	streamBase

	clientRequestedUsage bool
	// eventsEnabled is true when the caller opted into the
	// `X-Tinfoil-Events: web_search` marker stream. When false the
	// streamer emits only pure spec chat.completion.chunk frames; when
	// true it interleaves `<tinfoil-event>` markers inside delta.content.
	eventsEnabled bool
	emitter       *citationEmitter

	// roleEmitted is true after the initial assistant role-delta chunk has
	// been sent. The role chunk is deferred until the first upstream frame
	// is available so it can carry the upstream-provided id/created/model
	// rather than router-synthesized placeholders.
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
	toolCalls    []toolCall
	rawToolCalls []any
	finishReason string
	usage        map[string]any
}

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
			// Non-streaming postJSON hard-fails on invalid JSON;
			// streaming must do the same so silently lost chunks
			// (and therefore lost content / tool calls / usage) are
			// surfaced to the client as a terminating error instead
			// of a truncated success.
			return result, newUpstreamStreamError("upstream emitted malformed SSE JSON: " + jsonErr.Error())
		}
		// Upstream can emit a standalone error object mid-stream (for
		// example, context-length exceeded part-way through decoding).
		// Surface it as an upstream error the loop will translate into
		// a client-facing terminating error rather than silently hiding
		// the failure and continuing to consume frames.
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
	session, ok := registry.sessionFor(call.name)
	if !ok {
		return "", fmt.Errorf("no MCP session registered for tool %q", call.name)
	}
	switch call.name {
	case "search":
		id := "ws_" + uuid.NewString()
		action := map[string]any{"type": "search", "query": stringValue(call.arguments["query"])}
		s.emitTinfoilEventMarker(id, "in_progress", action, "")
		output, err := callTool(ctx, session, call.name, call.arguments, s.citations)
		if err != nil {
			s.emitTinfoilEventMarker(id, failureStatusFor(err), action, publicToolErrorReason(call.name, err))
			return "", err
		}
		s.emitTinfoilEventMarker(id, "completed", action, "")
		return output, nil
	case "fetch":
		urls := fetchArgumentURLs(call.arguments)
		if len(urls) == 0 {
			return callTool(ctx, session, call.name, call.arguments, s.citations)
		}
		// Pair one stable id per URL so opt-in clients can correlate
		// the in_progress marker with its matching terminal marker,
		// mirroring how the non-streaming shape surfaces fetches.
		fetchIDs := make([]string, len(urls))
		for i, url := range urls {
			fetchIDs[i] = "ws_" + uuid.NewString()
			s.emitTinfoilEventMarker(fetchIDs[i], "in_progress", map[string]any{"type": "open_page", "url": url}, "")
		}
		output, err := callTool(ctx, session, call.name, call.arguments, s.citations)
		if err != nil {
			reason := publicToolErrorReason(call.name, err)
			status := failureStatusFor(err)
			for i, url := range urls {
				s.emitTinfoilEventMarker(fetchIDs[i], status, map[string]any{"type": "open_page", "url": url}, reason)
			}
			return "", err
		}
		for i, url := range urls {
			s.emitTinfoilEventMarker(fetchIDs[i], "completed", map[string]any{"type": "open_page", "url": url}, "")
		}
		return output, nil
	default:
		return callTool(ctx, session, call.name, call.arguments, s.citations)
	}
}

// emitTinfoilEventMarker writes a single `<tinfoil-event>` marker as the
// `delta.content` of a chat.completion.chunk frame. When the caller did
// not opt into the marker stream, this is a no-op so strict SDKs see a
// pristine spec-conformant stream.
func (s *chatStreamer) emitTinfoilEventMarker(id, status string, action map[string]any, reason string) {
	if !s.eventsEnabled {
		return
	}
	marker := tinfoilEventMarker(id, status, action, reason)
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

// emitData writes one `data:` SSE frame to the client and flushes. The
// first write error (typically io.ErrClosedPipe once a client hangs up)
// is captured into s.writeErr; every subsequent emitData/emitEvent call
// is a no-op so the pump and outer loop can notice the disconnect and
// abort without spending more upstream tokens or MCP tool calls on a
// caller that is no longer listening.
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

// mustMarshal serializes the given value to JSON, returning an empty byte
// slice on failure. Used by error-surfacing helpers where failure to
// marshal is already a lost cause; returning empty body preserves the
// status-code signal on the upstream error.
func mustMarshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}


