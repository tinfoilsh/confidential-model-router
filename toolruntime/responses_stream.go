package toolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

	searchOpts := parseResponsesWebSearchOptions(body)
	tools := registry.allTools()
	ownedTools := registry.ownedTools()
	toolSchemas := schemaLookup(tools)
	base := cloneJSONMap(body)
	delete(base, "pii_check_options")
	delete(base, "prompt_injection_check_options")
	stripRouterOwnedIncludes(base)
	base["stream"] = true
	applyParallelToolCallsPolicy(base)
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

		routerToolCalls, clientToolCalls := splitToolCalls(ownedTools, result.toolCalls)
		if len(routerToolCalls) == 0 {
			return streamer.finalize(r, em, modelName, result)
		}

		// Execute every router-owned tool call so its citations and
		// web_search_call progress events are surfaced to the client
		// before any terminal events. On a non-mixed turn the outputs
		// are also appended to accumulatedInput so the next upstream
		// turn can see them; on a mixed turn the streamer finalizes
		// instead of looping so the function_call_output wrappers are
		// skipped.
		mixedTurn := len(clientToolCalls) > 0
		var toolOutputs []any
		if !mixedTurn {
			toolOutputs = make([]any, 0, len(routerToolCalls))
		}
		for _, call := range routerToolCalls {
			output := resolveStreamingRouterToolCall(
				ctx, call, searchOpts, toolSchemas, streamer.citations,
				func(ctx context.Context, call toolCall) (string, error) {
					return streamer.executeTool(ctx, registry, call)
				},
				"", "",
			)
			if !mixedTurn {
				toolOutputs = append(toolOutputs, map[string]any{
					"type":    "function_call_output",
					"call_id": call.id,
					"output":  output,
				})
			}
		}
		if mixedTurn {
			// Orphan-avoidance: appending the client function_calls to
			// accumulatedInput without matching function_call_output
			// items violates the Responses API contract and upstream
			// either rejects the next turn or silently drops the client
			// call. finalize emits response.completed with the resolved
			// web_search_call items (prepended by
			// attachResponsesCitations) alongside the client
			// function_calls already forwarded live.
			return streamer.finalize(r, em, modelName, result)
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

	// upstreamIDCaptured is true once the first upstream response id
	// has been adopted; later ids are ignored to keep the client's id
	// stable across iterations.
	upstreamIDCaptured bool
	responseID         string
	createdAt          int64

	// responseCreatedEmitted ensures exactly one response.created per
	// logical stream (finalize synthesizes one if upstream never did).
	responseCreatedEmitted bool

	// outputIndex is the next client-facing output_index. Upstream resets
	// its counter each iteration; the streamer re-bases so the client
	// sees a single monotonic sequence.
	outputIndex int

	// sequenceNumber is the streamer-scoped monotonic counter OpenAI's
	// spec requires on every envelope.
	sequenceNumber int

	// outputIndexMap rewrites upstream output_index to the client-facing
	// offset. Cleared between iterations.
	outputIndexMap map[int]int

	// suppressedItems is the set of upstream output_index values whose
	// items are router-owned function_calls resolved internally; their
	// events are dropped.
	suppressedItems map[int]struct{}

	// functionCallArguments buffers streamed function_call argument
	// fragments per upstream output_index. Some upstreams ship arguments
	// only via delta frames and deliver the item with an empty arguments
	// string, so the buffer is used to reassemble the full JSON.
	functionCallArguments map[int]*strings.Builder

	// emitters is one citationEmitter per (item_id, content_index)
	// tuple, so interleaved content parts index citations against the
	// correct text stream.
	emitters map[itemContentKey]*citationEmitter

	// annotationCounts is the next `annotation_index` per
	// (item_id, content_index) stream; the Responses event contract
	// requires annotation_index to be monotonic per text stream.
	annotationCounts map[itemContentKey]int

	// finalOutput accumulates the assistant output items from the final
	// iteration for echoing in the synthesized response.completed event.
	finalOutput []any

	// aggregatedUsage is the last non-nil usage block observed.
	aggregatedUsage map[string]any

	// ownedTools names the tools the router will execute itself; matching
	// function_call / mcp_call output items are suppressed from the
	// client stream. Client-owned tool calls are forwarded live.
	ownedTools map[string]struct{}

	// includeActionSources mirrors the request's opt-in for
	// `web_search_call.action.sources` on the terminal snapshot.
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
	body, err := s.openUpstreamSSE(ctx, em, modelName, "/v1/responses", reqBody, requestHeaders)
	if err != nil {
		return responsesIterationResult{}, err
	}
	defer body.Close()

	s.resetPerIterationState()
	return s.pumpUpstream(newSSEReader(body), isFirst)
}

// resetPerIterationState clears the fields whose scope is ONE upstream
// iteration. Upstream resets its output_index counter at the top of
// every turn, so the upstream->client index map, the suppression set,
// and the per-turn final-output snapshot must be cleared before each
// iteration. Everything else (client-facing counters, pinned identity,
// citations, usage accumulators) accumulates across iterations.
func (s *responsesStreamer) resetPerIterationState() {
	s.outputIndexMap = map[int]int{}
	s.suppressedItems = map[int]struct{}{}
	s.finalOutput = nil
}

// streamResponseID returns the pinned response id, materializing a
// router-minted fallback if upstream never sent one by the time it is
// first needed. The fallback is stored so every subsequent event
// references the same id. vLLM's /responses implementation always emits
// `id` on response.created, so hitting this fallback signals an upstream
// regression and is logged once per affected stream.
func (s *responsesStreamer) streamResponseID() string {
	if s.responseID == "" {
		s.responseID = "resp_" + uuid.NewString()
		log.Printf("toolruntime: upstream omitted response id on response.created, router minted fallback %s model=%s", s.responseID, s.model)
	}
	return s.responseID
}

// streamCreatedAt returns the pinned created_at timestamp, falling back to
// the router wall clock if upstream never sent a timestamp. Hitting this
// fallback is an OpenAI-spec violation from upstream; we heal the stream
// rather than fail it but log once per affected stream.
func (s *responsesStreamer) streamCreatedAt() int64 {
	if s.createdAt == 0 {
		s.createdAt = time.Now().Unix()
		log.Printf("toolruntime: upstream omitted response created_at, router stamped fallback %d model=%s", s.createdAt, s.model)
	}
	return s.createdAt
}

// streamModel returns the pinned model name captured from upstream;
// see chatStreamer.streamModel for the shared rationale.
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
// flushes. Latches the first write error into s.writeErr; see
// streamBase.writeErr. The sequence_number is always overwritten with
// the streamer's monotonic counter so upstream per-iteration resets
// cannot break the client-facing monotonicity invariant.
func (s *responsesStreamer) emitEvent(eventType string, body map[string]any) {
	if s.writeErr != nil {
		return
	}
	if body != nil {
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
			return result, newUpstreamStreamError("upstream emitted malformed SSE JSON: " + jsonErr.Error())
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
			return result, newUpstreamJSONError(map[string]any{"message": message})
		default:
			s.forwardEvent(eventType, event)
		}
	}
	if !terminalSeen {
		return result, newUpstreamStreamError("upstream stream ended without a terminal response event")
	}
	return result, nil
}
