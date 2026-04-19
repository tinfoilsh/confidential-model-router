package toolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// runResponsesStreaming is the streaming entry point for the Responses
// route. It owns the same tool loop as runResponsesLoop but consumes the
// upstream model's event stream incrementally and forwards output text
// deltas and annotations to the client live rather than synthesizing a
// single bundle at the end of the turn. When the client opted in via
// `X-Tinfoil-Events: web_search`, router-owned web_search tool-call
// progress is surfaced inline using `<tinfoil-event>...</tinfoil-event>`
// markers carried inside synthetic `response.output_text.delta` events.
// The synthetic carrier keeps the entire stream spec-conformant: OpenAI
// SDKs that do not recognize the marker format simply render the tagged
// JSON as assistant text; opt-in clients strip it with a single regex
// before rendering.
func runResponsesStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	em *manager.EnclaveManager,
	session *mcp.ClientSession,
	body map[string]any,
	modelName string,
	requestHeaders http.Header,
	prompt *mcp.GetPromptResult,
	tools []*mcp.Tool,
	ownedTools map[string]struct{},
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	searchOpts := parseResponsesWebSearchOptions(body)
	toolSchemas := schemaLookup(tools)
	base := cloneJSONMap(body)
	delete(base, "pii_check_options")
	delete(base, "prompt_injection_check_options")
	stripRouterOwnedIncludes(base)
	base["stream"] = true
	base["parallel_tool_calls"] = false
	base["tools"] = replaceResponsesWebSearchTools(base["tools"], responseTools(tools))
	base["input"] = prependResponsesPrompt(prompt, base["input"])

	usageMetricsRequested := r.Header.Get(manager.UsageMetricsRequestHeader) == "true"
	eventsEnabled := tinfoilEventsEnabled(r.Header)

	streamer := &responsesStreamer{
		w:                     w,
		flusher:               flusher,
		usageMetricsRequested: usageMetricsRequested,
		eventsEnabled:         eventsEnabled,
		citations:             &citationState{nextIndex: 1},
		usageTotals:           &usageAccumulator{},
		emitters:              map[itemContentKey]*citationEmitter{},
		annotationCounts:      map[itemContentKey]int{},
		functionCallArguments: map[int]*strings.Builder{},
		ownedTools:            ownedTools,
		includeActionSources:  searchOpts.includeActionSources,
	}

	accumulatedInput, _ := base["input"].([]any)
	reqBody := base

	for i := 0; i < maxToolIterations; i++ {
		isFirst := i == 0
		result, err := streamer.runIteration(ctx, em, modelName, reqBody, requestHeaders, isFirst)
		if err != nil {
			return streamer.terminateWithError(r, em, modelName, err)
		}
		// If the client disconnected mid-stream, stop before running
		// another tool call or upstream turn on behalf of a caller that
		// is no longer listening.
		if streamer.writeErr != nil {
			streamer.emitBillingEvent(r, em, modelName, streamer.totalsBillingUsage())
			return nil
		}

		routerToolCalls, _ := splitToolCalls(ownedTools, result.toolCalls)
		if len(routerToolCalls) == 0 {
			return streamer.finalize(r, em, modelName, result)
		}

		toolOutputs := make([]any, 0, len(routerToolCalls))
		for _, call := range routerToolCalls {
			applyWebSearchOptionsToToolCall(call.name, call.arguments, searchOpts)
			sanitizeToolCallArguments(call.name, call.arguments)
			coerceArgumentsToSchema(call.name, call.arguments, toolSchemas)
			output, toolErr := streamer.executeTool(ctx, session, call)
			record := toolCallRecord{
				name:      call.name,
				arguments: call.arguments,
			}
			if toolErr != nil {
				output = humanizeToolArgError(call.name, toolErr, call.arguments)
				record.errorReason = publicToolErrorReason(call.name, toolErr)
			} else {
				record.resultURLs = extractToolOutputURLs(output)
			}
			streamer.citations.recordToolCall(record)
			toolOutputs = append(toolOutputs, map[string]any{
				"type":    "function_call_output",
				"call_id": call.id,
				"output":  output,
			})
		}

		accumulatedInput = append(accumulatedInput, normalizeResponsesOutputItems(result.outputItems)...)
		accumulatedInput = append(accumulatedInput, toolOutputs...)
		reqBody = cloneJSONMap(base)
		reqBody["input"] = accumulatedInput
	}

	finalBody := forcedFinalResponsesRequest(reqBody)
	finalBody["stream"] = true
	result, err := streamer.runIteration(ctx, em, modelName, finalBody, requestHeaders, false)
	if err != nil {
		return streamer.terminateWithError(r, em, modelName, err)
	}
	return streamer.finalize(r, em, modelName, result)
}

// responsesStreamer owns the SSE stream sent back to the client across a
// whole logical Responses request. It reissues a single `response.created`
// event at the top of the first iteration, forwards upstream events with
// identity rewritten to a stable response id, executes router-owned tools
// synchronously between iterations, and emits one aggregated
// `response.completed` event at the very end.
type responsesStreamer struct {
	w                     http.ResponseWriter
	flusher               http.Flusher
	usageMetricsRequested bool
	// eventsEnabled is true when the caller opted into the
	// `X-Tinfoil-Events: web_search` marker stream. When false the
	// streamer emits only pure spec Responses events; when true it
	// additionally opens synthetic assistant-message items carrying
	// `<tinfoil-event>` markers inside output_text deltas.
	eventsEnabled bool
	citations     *citationState
	usageTotals   *usageAccumulator

	headersWritten bool
	// upstreamIDCaptured is true once we have adopted the first
	// upstream-provided response id. Subsequent upstream ids (from later
	// tool iterations) are ignored so the client sees one stable id.
	upstreamIDCaptured bool
	responseID         string
	createdAt          int64
	model              string
	fallbackModel      string

	// upstreamHeaders holds the most recent upstream response headers so
	// the billing event can surface the Tinfoil-Enclave attribution in
	// the same shape as the non-streaming path.
	upstreamHeaders http.Header

	// responseCreatedEmitted guards against duplicate response.created
	// events across iterations, and against emitting the event at all
	// when upstream never sends one (finalize synthesizes it in that
	// case so the client always sees exactly one).
	responseCreatedEmitted bool

	// outputIndex tracks the next output_index to assign to upstream output
	// items that we forward. Upstream resets to 0 on every iteration; we
	// offset so the client sees a monotonically increasing sequence.
	outputIndex int

	// sequenceNumber is the monotonically increasing counter OpenAI's spec
	// requires on every streaming envelope so clients can detect dropped
	// frames and order events across the whole response. It increments once
	// per emitted event across the entire streamed response.
	sequenceNumber int

	// outputIndexMap rewrites upstream output_index values on a given
	// iteration to the client-facing offset. Cleared between iterations.
	outputIndexMap map[int]int

	// suppressedItems holds the set of upstream output_index values whose
	// corresponding item is a router-owned function_call that we intend to
	// resolve internally. Events bearing those indices are dropped.
	suppressedItems map[int]struct{}

	// functionCallArguments buffers streamed function_call arguments keyed
	// by upstream output_index so we can reassemble the full JSON string
	// even when `response.output_item.done` arrives without embedded
	// arguments (some servers emit them only via the delta stream).
	functionCallArguments map[int]*strings.Builder

	// emitters holds one citationEmitter per (item_id, content_index) tuple
	// so citation spans are indexed against the right text stream when
	// upstream interleaves multiple content parts (for example reasoning
	// text followed by final answer text).
	emitters map[itemContentKey]*citationEmitter

	// annotationCounts tracks the next `annotation_index` to assign per
	// (item_id, content_index) stream so indexes are monotonic across
	// every emission batch, not just within a single delta's resolved
	// links. The Responses event contract specifies annotation_index as
	// a monotonic cursor per text stream.
	annotationCounts map[itemContentKey]int

	// finalOutput accumulates the assistant output items upstream emitted
	// on the final iteration so we can echo them in the synthesized
	// response.completed event.
	finalOutput []any

	// aggregatedUsage is the last non-nil usage block we observed, used by
	// response.completed and the billing event.
	aggregatedUsage map[string]any

	// ownedTools is the set of tool names the router will execute itself;
	// any function_call / mcp_call output item naming one of these is
	// suppressed from the client stream and resolved internally, while
	// function_calls naming client-owned tools are forwarded live.
	ownedTools map[string]struct{}

	// includeActionSources mirrors the request's opt-in for
	// `web_search_call.action.sources`. Propagated to
	// attachResponsesCitations when building the terminal
	// response.completed snapshot so the final output item carries the
	// sources array only when the caller asked for it.
	includeActionSources bool

	// writeErr latches the first error observed while writing to the
	// client SSE stream. Once set, emit* methods are no-ops and the
	// iteration / outer loops abort so the router does not keep running
	// upstream turns or MCP tool calls on behalf of a caller that has
	// already disconnected.
	writeErr error
}

type itemContentKey struct {
	itemID       string
	contentIndex int
}

// responsesIterationResult captures what one upstream streaming turn
// produced: the output items (assistant + any function_call items), the
// parsed tool calls from those items, and the usage block if provided.
type responsesIterationResult struct {
	outputItems []any
	toolCalls   []toolCall
	usage       map[string]any
}

// runIteration drives a single upstream streaming turn: opens the stream,
// forwards events to the client (rewriting identity where needed), and
// accumulates the output items and tool calls the model produced so the
// caller can either loop or finalize.
func (s *responsesStreamer) runIteration(
	ctx context.Context,
	em *manager.EnclaveManager,
	modelName string,
	reqBody map[string]any,
	requestHeaders http.Header,
	isFirst bool,
) (responsesIterationResult, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return responsesIterationResult{}, err
	}
	hdrs := cloneHeaders(requestHeaders)
	hdrs.Set("Content-Type", "application/json")
	hdrs.Set("Accept", "text/event-stream")

	resp, err := em.DoModelRequest(ctx, modelName, "/v1/responses", bodyBytes, hdrs)
	if err != nil {
		return responsesIterationResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return responsesIterationResult{}, &upstreamError{
			statusCode: resp.StatusCode,
			header:     resp.Header.Clone(),
			body:       errBody,
		}
	}
	s.upstreamHeaders = resp.Header
	if !s.headersWritten {
		s.writeSSEHeaders(resp.Header, modelName)
	}

	s.outputIndexMap = map[int]int{}
	s.suppressedItems = map[int]struct{}{}
	// Reset the terminal snapshot at the top of every iteration so the
	// final response.completed.output reflects ONLY what the last turn
	// emitted, matching the non-streaming runResponsesLoop semantics.
	// Intermediate iterations already streamed their items to the client
	// live; those items must not leak into the terminal snapshot a
	// client might use as the canonical final response.
	s.finalOutput = nil
	return s.pumpUpstream(newSSEReader(resp.Body), isFirst)
}

// writeSSEHeaders sets up the SSE response to the client once, the first
// time upstream returns a 2xx status. Identity fields are NOT
// pre-populated here: we wait until the first upstream event arrives so
// response.created and every subsequent event carry upstream's own
// id/created_at/model instead of router-minted placeholders. The
// fallback values are stashed and consulted lazily from the identity
// accessors below if upstream never sends a given field.
func (s *responsesStreamer) writeSSEHeaders(upstreamHeaders http.Header, fallbackModel string) {
	copyResponseHeaders(s.w.Header(), upstreamHeaders)
	s.w.Header().Del("Content-Length")
	s.w.Header().Del(manager.UsageMetricsResponseHeader)
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	if s.usageMetricsRequested {
		addTrailerHeader(s.w.Header(), manager.UsageMetricsResponseHeader)
	}
	s.w.WriteHeader(http.StatusOK)
	s.headersWritten = true
	s.fallbackModel = fallbackModel
}

// streamResponseID returns the pinned response id, materializing a
// router-minted fallback if upstream never sent one by the time it is
// first needed. The fallback is stored so every subsequent event
// references the same id.
func (s *responsesStreamer) streamResponseID() string {
	if s.responseID == "" {
		s.responseID = "resp_" + uuid.NewString()
	}
	return s.responseID
}

// streamCreatedAt returns the pinned created_at timestamp, falling back to
// the router wall clock if upstream never sent a timestamp.
func (s *responsesStreamer) streamCreatedAt() int64 {
	if s.createdAt == 0 {
		s.createdAt = time.Now().Unix()
	}
	return s.createdAt
}

// streamModel returns the pinned model name, falling back to the
// caller-requested label only if upstream never sent a model field.
func (s *responsesStreamer) streamModel() string {
	if s.model == "" {
		s.model = s.fallbackModel
	}
	return s.model
}

// pumpUpstream reads SSE events from upstream until the stream ends,
// forwarding events to the client with identity rewritten to s.responseID
// and output_index remapped to monotonically increasing values. Tool-call
// output items are captured for router-side execution rather than
// forwarded. response.completed is swallowed on non-final iterations
// because the streamer emits its own aggregated completion event. A bare
// EOF before any terminal marker (completed / failed / incomplete) is
// treated as an upstream disconnect and surfaced as an error so clients do
// not interpret a truncated turn as a successful answer.
// emitEvent writes one `event:`/`data:` SSE frame to the client and
// flushes. The first write error is captured into s.writeErr and every
// subsequent emitEvent call is a no-op so pumpUpstream and the outer
// loop notice the disconnect and stop spending tokens / MCP tool calls
// on a caller that has gone away.
func (s *responsesStreamer) emitEvent(eventType string, body map[string]any) {
	if s.writeErr != nil {
		return
	}
	if body != nil {
		// Always replace the sequence_number with a streamer-scoped counter.
		// Upstream enclaves reset their counter at the start of every
		// iteration of the tool loop, which would break the client-facing
		// invariant that sequence_number increases monotonically across the
		// whole response. Stamping our own counter here preserves that
		// guarantee even when we forward upstream events unchanged.
		body["sequence_number"] = s.sequenceNumber
		s.sequenceNumber++
	}
	if err := sseEvent(s.w, eventType, body); err != nil {
		s.writeErr = err
		return
	}
	s.flusher.Flush()
}

func (s *responsesStreamer) pumpUpstream(reader *sseReader, isFirst bool) (responsesIterationResult, error) {
	result := responsesIterationResult{}
	terminalSeen := false
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
		if frame.data == "" || frame.data == "[DONE]" {
			continue
		}
		var event map[string]any
		if jsonErr := json.Unmarshal([]byte(frame.data), &event); jsonErr != nil {
			// Mirror the chat streamer's hard-fail policy: malformed
			// upstream JSON means we have silently lost at least one
			// output item, delta, or usage block, which is
			// indistinguishable from a correct empty turn. Surface it
			// as an error rather than letting the client believe the
			// truncated stream was complete.
			return result, &upstreamError{
				statusCode: http.StatusBadGateway,
				header:     http.Header{"Content-Type": []string{"application/json"}},
				body: mustMarshal(map[string]any{
					"error": map[string]any{
						"message": "upstream emitted malformed SSE JSON: " + jsonErr.Error(),
						"type":    "upstream_error",
					},
				}),
			}
		}
		eventType := stringValue(event["type"])
		if eventType == "" {
			eventType = frame.event
		}
		s.captureIdentity(event)

		switch eventType {
		case "response.created":
			if isFirst {
				s.ensureResponseCreated()
			}
		case "response.in_progress":
			// Silently consumed; the synthesized response.created already
			// communicated in_progress status to the client.
		case "response.output_item.added":
			s.handleOutputItemAdded(event)
		case "response.output_item.done":
			s.handleOutputItemDone(event, &result)
		case "response.output_text.delta":
			s.handleOutputTextDelta(event)
		case "response.output_text.done":
			s.handleOutputTextDone(event)
		case "response.function_call_arguments.delta":
			upstreamIndex := int(numberValue(event["output_index"]))
			// Accumulate the argument fragments regardless of ownership so
			// handleOutputItemDone can reassemble them when the terminal
			// `output_item.done` event carries empty arguments. This is a
			// documented quirk of the Responses streaming protocol: some
			// upstreams ship function arguments only as delta fragments
			// and deliver the item itself with an empty arguments string.
			// Router-owned calls still need the buffer for internal
			// execution; client-owned calls need it so the synthesized
			// response.completed.output echoes a usable tool call.
			s.handleFunctionCallArgumentsDelta(event)
			if _, suppressed := s.suppressedItems[upstreamIndex]; !suppressed {
				s.forwardEvent(eventType, event)
			}
		case "response.function_call_arguments.done":
			upstreamIndex := int(numberValue(event["output_index"]))
			if _, suppressed := s.suppressedItems[upstreamIndex]; !suppressed {
				s.forwardEvent(eventType, event)
			}
		case "response.completed":
			terminalSeen = true
			if usage := extractResponsesUsage(event); usage != nil {
				result.usage = usage
				s.aggregatedUsage = usage
				s.usageTotals.Add(&upstreamJSONResponse{body: map[string]any{"usage": usage}})
			}
		case "response.failed", "response.incomplete":
			terminalSeen = true
			if usage := extractResponsesUsage(event); usage != nil {
				result.usage = usage
				s.aggregatedUsage = usage
				s.usageTotals.Add(&upstreamJSONResponse{body: map[string]any{"usage": usage}})
			}
			// Upstream gave up on this turn. Surface it as an error to
			// the caller so the loop stops cleanly and the client sees
			// a response.failed rather than silently continuing into
			// another iteration that has nothing to resolve.
			message := extractResponsesErrorMessage(event)
			if message == "" {
				message = "upstream response " + eventType
			}
			return result, &upstreamError{
				statusCode: http.StatusBadGateway,
				header:     http.Header{"Content-Type": []string{"application/json"}},
				body:       mustMarshal(map[string]any{"error": map[string]any{"message": message}}),
			}
		default:
			s.forwardEvent(eventType, event)
		}
	}
	if !terminalSeen {
		return result, &upstreamError{
			statusCode: http.StatusBadGateway,
			header:     http.Header{"Content-Type": []string{"application/json"}},
			body: mustMarshal(map[string]any{
				"error": map[string]any{
					"message": "upstream stream ended without a terminal response event",
					"type":    "upstream_error",
				},
			}),
		}
	}
	return result, nil
}

// captureIdentity records the upstream-provided response id, creation
// time and model on the first event that carries each one. Subsequent
// iterations retain the first captured value per field so the client sees
// one stable logical stream across every tool turn, and so the
// synthesized response.created and response.completed events carry
// upstream's real created_at instead of the router's wall clock.
func (s *responsesStreamer) captureIdentity(event map[string]any) {
	response, _ := event["response"].(map[string]any)
	if response == nil {
		return
	}
	if !s.upstreamIDCaptured {
		if id := stringValue(response["id"]); id != "" {
			s.responseID = id
			s.upstreamIDCaptured = true
		}
	}
	if s.createdAt == 0 {
		if created := int64(numberValue(response["created_at"])); created > 0 {
			s.createdAt = created
		}
	}
	if s.model == "" {
		if model := stringValue(response["model"]); model != "" {
			s.model = model
		}
	}
}

// ensureResponseCreated issues the synthesized response.created event
// using the streamer's stable identity fields, exactly once per logical
// stream. Callers may invoke it from the first upstream `response.created`
// frame (in which case the identity accessors return upstream's values)
// or from finalize (defensive, for upstreams that never send one). The
// accessors fall back to router-minted defaults only when strictly
// needed.
func (s *responsesStreamer) ensureResponseCreated() {
	if s.responseCreatedEmitted {
		return
	}
	s.responseCreatedEmitted = true
	s.emitEvent("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         s.streamResponseID(),
			"object":     "response",
			"created_at": s.streamCreatedAt(),
			"status":     "in_progress",
			"model":      s.streamModel(),
			"output":     []any{},
		},
	})
}

// handleOutputItemAdded forwards the event to the client after remapping
// output_index to the streamer's monotonically increasing namespace, or
// suppresses it entirely if the item represents a router-owned tool call
// we will execute ourselves. Item accumulation happens on the matching
// `_done` event so this handler does not need the iteration result.
//
// Client-owned function_call items are forwarded live so the calling SDK
// can act on them (e.g., execute the tool on its side and post results).
// Only tool names listed in s.ownedTools are suppressed; unknown tools
// default to client-owned to match the non-streaming splitToolCalls
// classification.
func (s *responsesStreamer) handleOutputItemAdded(event map[string]any) {
	item, _ := event["item"].(map[string]any)
	upstreamIndex := int(numberValue(event["output_index"]))
	if s.isRouterOwnedToolItem(item) {
		// Router-owned tool call; intercept for later resolution and do
		// not surface the intermediate tool invocation to the client.
		// mcp_call is included because some upstreams model the same
		// function invocation under the MCP output shape when tools are
		// presented via the legacy MCP path, and we want both to be
		// resolved identically.
		s.suppressedItems[upstreamIndex] = struct{}{}
		return
	}
	clientIndex := s.outputIndex
	s.outputIndexMap[upstreamIndex] = clientIndex
	s.outputIndex++
	payload := cloneJSONMap(event)
	payload["output_index"] = clientIndex
	s.emitEvent("response.output_item.added", payload)
}

// isRouterOwnedToolItem reports whether the given output item is a tool
// invocation this router will execute internally. Any function_call or
// mcp_call whose name is in the configured ownedTools set is suppressed
// from the client stream; everything else (including function_calls for
// tools the caller declared themselves) is forwarded verbatim.
func (s *responsesStreamer) isRouterOwnedToolItem(item map[string]any) bool {
	if item == nil {
		return false
	}
	itemType := stringValue(item["type"])
	if itemType != "function_call" && itemType != "mcp_call" {
		return false
	}
	if s.ownedTools == nil {
		return false
	}
	_, ok := s.ownedTools[stringValue(item["name"])]
	return ok
}

// handleOutputItemDone completes the client-facing view of an item and
// appends the finished item to finalOutput so response.completed can echo
// every output the model produced. For BOTH router-owned and client-owned
// function calls the parsed arguments are recorded for the loop; when the
// upstream `done` event lacks arguments (some servers ship them only via
// the delta stream) we fall back to the per-index buffer accumulated from
// `response.function_call_arguments.delta` frames. For client-owned
// calls the rebuilt arguments string is also written back into the item
// so the forwarded `output_item.done` event and the terminal
// `response.completed.output` both carry a usable tool call.
func (s *responsesStreamer) handleOutputItemDone(event map[string]any, result *responsesIterationResult) {
	item, _ := event["item"].(map[string]any)
	upstreamIndex := int(numberValue(event["output_index"]))
	itemType := stringValue(item["type"])
	if s.isRouterOwnedToolItem(item) {
		arguments := parseArguments(item["arguments"])
		if len(arguments) == 0 {
			if buf, ok := s.functionCallArguments[upstreamIndex]; ok {
				arguments = parseArguments(buf.String())
				// Splice the reassembled JSON back into the item before
				// it lands in result.outputItems. The outer loop replays
				// those items into the next /v1/responses turn's input,
				// and some upstream variants ship arguments only via
				// delta frames -- without this splice the replayed item
				// would carry an empty arguments string and the model
				// would see its own prior tool call as argument-less.
				if rawArgs := stringValue(item["arguments"]); rawArgs == "" {
					item["arguments"] = buf.String()
				}
			}
		}
		delete(s.functionCallArguments, upstreamIndex)
		result.outputItems = append(result.outputItems, item)
		callID := stringValue(item["call_id"])
		if callID == "" {
			callID = stringValue(item["id"])
		}
		result.toolCalls = append(result.toolCalls, toolCall{
			id:        callID,
			name:      stringValue(item["name"]),
			arguments: arguments,
		})
		return
	}
	if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
		return
	}
	// Client-owned function_calls: reassemble arguments from the delta
	// buffer when the item itself ships with an empty arguments string,
	// and splice the reassembled form back into the item so downstream
	// consumers see a complete tool call.
	if itemType == "function_call" || itemType == "mcp_call" {
		if rawArgs := stringValue(item["arguments"]); rawArgs == "" {
			if buf, ok := s.functionCallArguments[upstreamIndex]; ok && buf.Len() > 0 {
				item["arguments"] = buf.String()
			}
		}
		delete(s.functionCallArguments, upstreamIndex)
	}
	clientIndex, ok := s.outputIndexMap[upstreamIndex]
	if !ok {
		clientIndex = s.outputIndex
		s.outputIndexMap[upstreamIndex] = clientIndex
		s.outputIndex++
	}
	payload := cloneJSONMap(event)
	payload["output_index"] = clientIndex
	s.emitEvent("response.output_item.done", payload)
	s.finalOutput = append(s.finalOutput, item)
	result.outputItems = append(result.outputItems, item)
	if itemType == "function_call" || itemType == "mcp_call" {
		// Client-owned tool call: propagate to iteration result so
		// splitToolCalls classifies it as a client tool call and the
		// outer loop falls through to finalize rather than attempting to
		// execute it internally.
		callID := stringValue(item["call_id"])
		if callID == "" {
			callID = stringValue(item["id"])
		}
		result.toolCalls = append(result.toolCalls, toolCall{
			id:        callID,
			name:      stringValue(item["name"]),
			arguments: parseArguments(item["arguments"]),
		})
	}
}

// handleFunctionCallArgumentsDelta accumulates partial JSON arguments for
// a router-owned function_call item that the loop will resolve internally.
// The buffered string is consumed when the matching
// `response.output_item.done` event fires.
func (s *responsesStreamer) handleFunctionCallArgumentsDelta(event map[string]any) {
	upstreamIndex := int(numberValue(event["output_index"]))
	delta := stringValue(event["delta"])
	if delta == "" {
		return
	}
	if s.functionCallArguments == nil {
		s.functionCallArguments = map[int]*strings.Builder{}
	}
	buf, ok := s.functionCallArguments[upstreamIndex]
	if !ok {
		buf = &strings.Builder{}
		s.functionCallArguments[upstreamIndex] = buf
	}
	buf.WriteString(delta)
}

// handleOutputTextDelta pushes the incoming text fragment through the
// content's citation emitter so inline markdown links resolve to
// annotation events, and forwards remaining text to the client as an
// output_text.delta event.
func (s *responsesStreamer) handleOutputTextDelta(event map[string]any) {
	upstreamIndex := int(numberValue(event["output_index"]))
	if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
		return
	}
	clientIndex, ok := s.outputIndexMap[upstreamIndex]
	if !ok {
		clientIndex = s.outputIndex
		s.outputIndexMap[upstreamIndex] = clientIndex
		s.outputIndex++
	}
	itemID := stringValue(event["item_id"])
	contentIndex := int(numberValue(event["content_index"]))
	key := itemContentKey{itemID: itemID, contentIndex: contentIndex}
	emitter, exists := s.emitters[key]
	if !exists {
		emitter = newCitationEmitter(s.citations)
		s.emitters[key] = emitter
	}
	delta := stringValue(event["delta"])
	contentChunk, annotations := emitter.push(delta)
	if contentChunk != "" {
		s.emitEvent("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"output_index":  clientIndex,
			"item_id":       itemID,
			"content_index": contentIndex,
			"delta":         contentChunk,
		})
	}
	s.emitAnnotationEvents(annotations, clientIndex, itemID, contentIndex)
}

// handleOutputTextDone flushes any content still held in the emitter's
// trailing buffer so the user sees every rune before the item closes, then
// forwards the done event with identity rewritten.
func (s *responsesStreamer) handleOutputTextDone(event map[string]any) {
	upstreamIndex := int(numberValue(event["output_index"]))
	if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
		return
	}
	clientIndex, ok := s.outputIndexMap[upstreamIndex]
	if !ok {
		clientIndex = s.outputIndex
		s.outputIndexMap[upstreamIndex] = clientIndex
		s.outputIndex++
	}
	itemID := stringValue(event["item_id"])
	contentIndex := int(numberValue(event["content_index"]))
	key := itemContentKey{itemID: itemID, contentIndex: contentIndex}
	if emitter, ok := s.emitters[key]; ok {
		contentChunk, annotations := emitter.flush()
		if contentChunk != "" {
			s.emitEvent("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"output_index":  clientIndex,
				"item_id":       itemID,
				"content_index": contentIndex,
				"delta":         contentChunk,
			})
		}
		s.emitAnnotationEvents(annotations, clientIndex, itemID, contentIndex)
		delete(s.emitters, key)
	}
	payload := cloneJSONMap(event)
	payload["output_index"] = clientIndex
	s.emitEvent("response.output_text.done", payload)
}

// emitAnnotationEvents turns resolved link matches into the documented
// Responses annotation event shape: `response.output_text.annotation.added`
// with a flat url_citation attached to the item/content coordinates the
// matching text lives at. The `annotation_index` is a monotonic cursor
// per (item_id, content_index) pair so clients can use it to dedupe or
// order annotations across the full text stream, not just within the
// single push/flush batch that happened to resolve them.
func (s *responsesStreamer) emitAnnotationEvents(matches []citationAnnotation, outputIndex int, itemID string, contentIndex int) {
	if len(matches) == 0 {
		return
	}
	if s.annotationCounts == nil {
		s.annotationCounts = map[itemContentKey]int{}
	}
	key := itemContentKey{itemID: itemID, contentIndex: contentIndex}
	for _, match := range matches {
		annotation := map[string]any{
			"type":        "url_citation",
			"url":         match.source.url,
			"start_index": match.startIndex,
			"end_index":   match.endIndex,
		}
		if match.source.title != "" {
			annotation["title"] = match.source.title
		}
		annotationIndex := s.annotationCounts[key]
		s.annotationCounts[key] = annotationIndex + 1
		s.emitEvent("response.output_text.annotation.added", map[string]any{
			"type":             "response.output_text.annotation.added",
			"output_index":     outputIndex,
			"item_id":          itemID,
			"content_index":    contentIndex,
			"annotation_index": annotationIndex,
			"annotation":       annotation,
		})
	}
}

// forwardEvent relays an upstream event the streamer does not specially
// handle, rewriting output_index when present so the client always sees
// the streamer's canonical monotonic namespace.
func (s *responsesStreamer) forwardEvent(eventType string, event map[string]any) {
	if rawIndex, ok := event["output_index"]; ok {
		upstreamIndex := int(numberValue(rawIndex))
		if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
			return
		}
		if clientIndex, mapped := s.outputIndexMap[upstreamIndex]; mapped {
			event = cloneJSONMap(event)
			event["output_index"] = clientIndex
		}
	}
	s.emitEvent(eventType, event)
}

// executeTool invokes the MCP session and, when the caller opted into
// the router-owned marker stream via `X-Tinfoil-Events: web_search`,
// surfaces search / fetch progress as `<tinfoil-event>` markers carried
// inside synthetic assistant-message output items. Strict OpenAI SDKs
// see only spec-conformant events (a message item opens, emits a text
// delta, and closes); opt-in clients parse the marker out of that text.
// When events are not enabled this method is a no-op wrapper around
// callTool, preserving the pristine spec-conformant stream expected by
// callers that did not set the header.
func (s *responsesStreamer) executeTool(ctx context.Context, session *mcp.ClientSession, call toolCall) (string, error) {
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

// emitTinfoilEventMarker writes one `<tinfoil-event>` marker to the
// client by opening a synthetic assistant-message output item, emitting
// its text delta, and closing the item. The five-event burst is
// spec-conformant on every frame — opt-out clients see a tiny assistant
// message whose text happens to contain `<tinfoil-event>...</tinfoil-event>`
// which they will simply render as-is. Opt-in clients strip the tags
// before rendering. When the caller did not opt in this is a no-op so
// strict clients see no extra frames at all.
//
// The synthetic item participates in the streamer's normal output_index
// bookkeeping so any subsequent real items keep a monotonically
// increasing index sequence. The item is NOT accumulated into finalOutput
// because the terminal `response.completed` snapshot uses web_search_call
// output items as the canonical progress surface (via
// attachResponsesCitations).
func (s *responsesStreamer) emitTinfoilEventMarker(id, status string, action map[string]any, reason string) {
	if !s.eventsEnabled {
		return
	}
	marker := tinfoilEventMarker(id, status, action, reason)
	itemID := "msg_tf_" + uuid.NewString()
	outputIndex := s.outputIndex
	s.outputIndex++

	item := map[string]any{
		"id":     itemID,
		"type":   "message",
		"role":   "assistant",
		"status": "in_progress",
		"content": []any{
			map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		},
	}
	s.emitEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item":         item,
	})
	s.emitEvent("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       itemID,
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	s.emitEvent("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       itemID,
		"output_index":  outputIndex,
		"content_index": 0,
		"delta":         marker,
	})
	s.emitEvent("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       itemID,
		"output_index":  outputIndex,
		"content_index": 0,
		"text":          marker,
	})
	s.emitEvent("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       itemID,
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": marker, "annotations": []any{}},
	})
	completedItem := cloneJSONMap(item)
	completedItem["status"] = "completed"
	completedItem["content"] = []any{
		map[string]any{"type": "output_text", "text": marker, "annotations": []any{}},
	}
	s.emitEvent("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
		"item":         completedItem,
	})
}

// finalize writes the aggregated `response.completed` event with every
// output item the model produced plus the accumulated usage, fires the
// billing event, and closes the SSE stream.
func (s *responsesStreamer) finalize(r *http.Request, em *manager.EnclaveManager, modelName string, result responsesIterationResult) error {
	// Cover the degenerate case where upstream never emitted a
	// response.created frame (or emitted it on an iteration other than
	// the first, which we ignore). The client still needs to see exactly
	// one response.created before any response.completed.
	s.ensureResponseCreated()

	// By the time finalize runs, every text part has received its
	// matching output_text.done, which already drained the emitter for
	// that (item_id, content_index). Anything still in s.emitters is
	// necessarily empty; clearing the map ensures we do not leak state
	// across requests if the streamer is ever reused.
	s.emitters = map[itemContentKey]*citationEmitter{}

	usage := result.usage
	if usage == nil {
		usage = s.aggregatedUsage
	}
	totals := s.usageTotals.Usage()
	if totals != nil {
		usage = map[string]any{
			"input_tokens":  totals.PromptTokens,
			"output_tokens": totals.CompletionTokens,
			"total_tokens":  totals.TotalTokens,
		}
	}
	// Build the final `output` array. Run it through attachResponsesCitations
	// on a synthetic body so the final snapshot carries the same normalized
	// text and the same flat url_citation annotations the non-streaming path
	// produces, plus the prepended web_search_call items for every
	// router-owned tool call. Clients that consume response.completed
	// instead of (or in addition to) the live delta stream must see a
	// terminal payload that is byte-for-byte comparable to the
	// non-streaming response.
	completedBody := map[string]any{"output": append([]any{}, s.finalOutput...)}
	// Streaming already fired tinfoil-event markers live via synthetic
	// output_text deltas, so the terminal snapshot must NOT prepend
	// them again (the client would otherwise see a duplicate rendering
	// in `response.completed.output`). Pass eventsEnabled=false here
	// regardless of header opt-in.
	attachResponsesCitations(completedBody, s.citations, s.includeActionSources, false)
	finalOutput, _ := completedBody["output"].([]any)
	completed := map[string]any{
		"id":         s.streamResponseID(),
		"object":     "response",
		"created_at": s.streamCreatedAt(),
		"status":     "completed",
		"model":      s.streamModel(),
		"output":     finalOutput,
	}
	if usage != nil {
		completed["usage"] = usage
	}
	s.emitEvent("response.completed", map[string]any{
		"type":     "response.completed",
		"response": completed,
	})

	if s.usageMetricsRequested && usage != nil {
		s.w.Header().Set(manager.UsageMetricsResponseHeader, formatUsageHeader(usageFromRawMap(usage)))
	}
	s.emitBillingEvent(r, em, modelName, usage)
	return s.writeErr
}

// emitBillingEvent delegates to the shared billing emitter used by the
// non-streaming path so streaming and non-streaming requests produce the
// same billing-event shape, including Tinfoil-Enclave attribution taken
// from the most recent upstream response headers.
func (s *responsesStreamer) emitBillingEvent(r *http.Request, em *manager.EnclaveManager, modelName string, usage map[string]any) {
	if em == nil {
		return
	}
	header := s.upstreamHeaders
	if header == nil {
		header = http.Header{}
	}
	response := &upstreamJSONResponse{
		header: header,
		body:   map[string]any{"usage": usage},
	}
	emitBillingEvent(em, r, response, modelName, true)
}

// terminateWithError surfaces a mid-stream upstream failure to the client.
// If no SSE has started, the error bubbles up so Handle can turn it into a
// plain JSON error response; otherwise the streamer emits a synthesized
// response.failed event and the billing pipeline records every token
// accumulated across every successful turn so far, not just the failing
// one. Multi-turn tool loops would otherwise undercount billing when a
// later iteration fails.
func (s *responsesStreamer) terminateWithError(r *http.Request, em *manager.EnclaveManager, modelName string, err error) error {
	if !s.headersWritten {
		return err
	}
	s.ensureResponseCreated()
	s.emitEvent("response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id":         s.streamResponseID(),
			"object":     "response",
			"created_at": s.streamCreatedAt(),
			"status":     "failed",
			"model":      s.streamModel(),
			"output":     s.finalOutput,
			"error":      upstreamErrorPayload(err),
		},
	})
	s.emitBillingEvent(r, em, modelName, s.totalsBillingUsage())
	return nil
}

// totalsBillingUsage returns the aggregated Responses-shaped usage block
// spanning every iteration the stream has consumed, falling back to the
// most recent single-turn usage block if no totals were recorded.
func (s *responsesStreamer) totalsBillingUsage() map[string]any {
	if totals := s.usageTotals.Usage(); totals != nil {
		return map[string]any{
			"input_tokens":  totals.PromptTokens,
			"output_tokens": totals.CompletionTokens,
			"total_tokens":  totals.TotalTokens,
		}
	}
	return s.aggregatedUsage
}

// extractResponsesUsage pulls the usage block out of a response.completed
// / response.failed / response.incomplete event, where it lives under the
// nested `response` object per the documented Responses API event shape.
func extractResponsesUsage(event map[string]any) map[string]any {
	response, _ := event["response"].(map[string]any)
	if response == nil {
		return nil
	}
	usage, _ := response["usage"].(map[string]any)
	return usage
}

// extractResponsesErrorMessage pulls a human-readable reason out of a
// response.failed or response.incomplete event. Upstream may place the
// detail under response.error.message or response.incomplete_details;
// callers treat an empty return as "no extra detail available" and fall
// back to the event type.
func extractResponsesErrorMessage(event map[string]any) string {
	response, _ := event["response"].(map[string]any)
	if response == nil {
		return ""
	}
	if errObj, ok := response["error"].(map[string]any); ok {
		if message := stringValue(errObj["message"]); message != "" {
			return message
		}
	}
	if details, ok := response["incomplete_details"].(map[string]any); ok {
		if reason := stringValue(details["reason"]); reason != "" {
			return reason
		}
	}
	return ""
}
