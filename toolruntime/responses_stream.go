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
// single bundle at the end of the turn. Router-owned web_search tool
// progress is surfaced via the spec-defined streaming events OpenAI
// documents for the Responses API: response.output_item.added carrying
// a web_search_call item, followed by response.web_search_call.*
// progress envelopes and a terminal response.output_item.done. No
// tinfoil-specific channel rides on this route -- the Responses stream
// is always fully OpenAI-spec-conformant.
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

	streamer := &responsesStreamer{
		streamBase: streamBase{
			w:                     w,
			flusher:               flusher,
			usageMetricsRequested: usageMetricsRequested,
			citations:             &citationState{nextIndex: 1},
			usageTotals:           &usageAccumulator{},
		},
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
	// streamBase holds shared state (client writer, flusher, citations,
	// usage totals, headers-written flag, upstream headers, pinned
	// model name, latched writeErr) common to every SSE streamer. See
	// stream_base.go for the full documentation.
	streamBase

	// upstreamIDCaptured is true once we have adopted the first
	// upstream-provided response id. Subsequent upstream ids (from later
	// tool iterations) are ignored so the client sees one stable id.
	upstreamIDCaptured bool
	responseID         string
	createdAt          int64

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
		s.writeSSEHeaders(resp.Header)
	}

	s.resetPerIterationState()
	return s.pumpUpstream(newSSEReader(resp.Body), isFirst)
}

// resetPerIterationState clears the fields whose scope is ONE upstream
// iteration, not the whole logical response. Upstream resets its own
// output_index counter at the top of every tool-loop turn, so the
// router's upstream->client index rewrite table and the suppression
// set must also be cleared before each iteration or mappings from the
// previous turn would collide with fresh upstream indices.
//
// Scope boundaries (fields intentionally NOT touched here):
//   - outputIndex / sequenceNumber: advance across iterations because
//     they address the CLIENT-facing stream, which is one logical
//     response spanning every upstream turn.
//   - citations / emitters / annotationCounts: accumulate across
//     iterations so citation numbering is stable end-to-end.
//   - usageTotals / aggregatedUsage: aggregate across iterations so
//     finalize emits one authoritative usage block.
//   - responseID / createdAt / model / upstreamIDCaptured: captured
//     from the first upstream turn and pinned; every client-facing
//     frame must reference the same identity.
//   - headersWritten / responseCreatedEmitted: once-per-stream gates.
//   - functionCallArguments: keyed by upstream index and deleted on
//     item-done, so stale entries cannot accumulate across iterations.
//
// Resetting the final-output snapshot is part of this same boundary:
// response.completed must reflect only the items the terminal turn
// emitted, matching the non-streaming runResponsesLoop semantics;
// intermediate iterations already streamed their items to the client
// live and must not leak into the terminal snapshot a client might
// replay as the canonical response.
func (s *responsesStreamer) resetPerIterationState() {
	s.outputIndexMap = map[int]int{}
	s.suppressedItems = map[int]struct{}{}
	s.finalOutput = nil
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

// streamModel returns the pinned model name captured from upstream.
// Every OpenAI-compatible inference server echoes request.model on
// response.created; a missing value indicates an upstream bug and is
// surfaced as a loud stream error via streamBase.validateStreamModel
// so we do not mask a fleet regression behind a cached config label.
func (s *responsesStreamer) streamModel() string {
	return s.validateStreamModel("response.model")
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
