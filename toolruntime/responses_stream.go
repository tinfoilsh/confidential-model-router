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

// ---------------------------------------------------------------------------
// responsesStreamer methods
// ---------------------------------------------------------------------------

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
	s.functionCallArguments = map[int]*strings.Builder{}
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

// executeTool invokes the MCP session and surfaces router-owned web
// search progress as the spec-conformant Responses streaming events
// documented by OpenAI:
//
//   - response.output_item.added (item type web_search_call, status in_progress)
//   - response.web_search_call.in_progress
//   - response.web_search_call.searching (search only; fetch omits this phase)
//   - response.web_search_call.completed (on success) OR no dedicated event on failure
//   - response.output_item.done (terminal status completed or failed)
//
// The stream is always fully spec-conformant on the Responses path; no
// opt-in marker channel is used. Clients that want to distinguish a
// safety-filter block from a generic failure can inspect the non-spec
// `status` strings on the terminal `web_search_call` output item carried
// in `response.completed.output`, which is collapsed onto `failed` at
// the envelope level but preserved on the record for tooling.
func (s *responsesStreamer) executeTool(ctx context.Context, registry *sessionRegistry, call toolCall) (string, error) {
	return executeToolWithProgress(ctx, registry, s.citations, &responsesToolProgressEmitter{streamer: s}, call)
}

// openWebSearchCallItem emits response.output_item.added for a
// router-owned web_search_call item and returns the output_index the
// client will see for it. The item is NOT recorded into s.finalOutput
// because attachResponsesOutput rebuilds the terminal snapshot from
// s.toolCalls; the live event here is solely for the client's
// progress UI.
func (s *responsesStreamer) openWebSearchCallItem(id string, action map[string]any) int {
	outputIndex := s.outputIndex
	s.outputIndex++
	s.emitEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item": map[string]any{
			"id":     id,
			"type":   "web_search_call",
			"status": "in_progress",
			"action": action,
		},
	})
	return outputIndex
}

// emitToolCallPhase emits a progress envelope for a web_search_call
// item in the shape OpenAI's spec defines: {item_id, output_index,
// type}. The eventType determines the specific phase (e.g.
// response.web_search_call.searching).
func (s *responsesStreamer) emitToolCallPhase(eventType, itemID string, outputIndex int) {
	s.emitEvent(eventType, map[string]any{
		"type":         eventType,
		"item_id":      itemID,
		"output_index": outputIndex,
	})
}

// closeWebSearchCallItem emits response.output_item.done for a
// router-owned web_search_call item with the given terminal status. The
// spec defines the envelope status enum as {in_progress, searching,
// completed, failed}, so any internal `blocked` value is collapsed
// inside webSearchCallEvent and the unfiltered router status (plus any
// error code) rides on the `_tinfoil` sidecar for clients that want to
// render a distinct affordance for safety-filter blocks.
func (s *responsesStreamer) closeWebSearchCallItem(id string, outputIndex int, action map[string]any, status, errorCode string) {
	item := webSearchCallEvent(id, status, errorCode, action)
	s.emitEvent("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
		"item":         item,
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
	// Build the final `output` array. Run it through the citation attachment
	// on a synthetic body so the final snapshot carries the same normalized
	// text and the same flat url_citation annotations the non-streaming path
	// produces, plus the prepended web_search_call items for every
	// router-owned tool call. Clients that consume response.completed
	// instead of (or in addition to) the live delta stream must see a
	// terminal payload that is byte-for-byte comparable to the
	// non-streaming response.
	completedBody := map[string]any{"output": append([]any{}, s.finalOutput...)}
	// The Responses streaming path surfaces web_search progress via the
	// spec-defined `response.web_search_call.*` events fired live from
	// executeTool and via the prepended `web_search_call` output items
	// attachResponsesOutput injects here. No tinfoil-event
	// marker channel rides on this path -- the Responses stream is
	// always fully OpenAI-spec-conformant.
	attachResponsesOutput(completedBody, s.citations, s.toolCalls, s.includeActionSources)
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
		s.w.Header().Set(manager.UsageMetricsResponseHeader, formatUsageHeader(usageFromRaw(usage)))
	}
	s.emitBillingEvent(r, em, modelName, usage)
	return s.writeErr
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

// ---------------------------------------------------------------------------
// Free functions
// ---------------------------------------------------------------------------

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
	dl *devLog,
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
	base["tools"] = replaceRouterOwnedResponsesTools(base["tools"], responseTools(tools))
	base["input"] = prependResponsesPrompt(prompt, base["input"])

	usageMetricsRequested := r.Header.Get(manager.UsageMetricsRequestHeader) == "true"

	streamer := &responsesStreamer{
		streamBase: streamBase{
			w:                     w,
			flusher:               flusher,
			usageMetricsRequested: usageMetricsRequested,
			citations:             &citationState{nextIndex: 1},
			toolCalls:             &toolCallLog{},
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

		dl.WriteStreamedTurn(i+1, result.usage, "", "")

		routerToolCalls, clientToolCalls := splitToolCalls(ownedTools, result.toolCalls)
		if len(routerToolCalls) == 0 {
			dl.WriteFinish("stop (no router tool calls)")
			return streamer.finalize(r, em, modelName, result)
		}

		dl.WriteToolCalls(routerToolCalls)
		mixedTurn := len(clientToolCalls) > 0
		var toolOutputs []any
		if !mixedTurn {
			toolOutputs = make([]any, 0, len(routerToolCalls))
		}
		for _, call := range routerToolCalls {
			tstart := time.Now()
			output := resolveStreamingRouterToolCall(
				ctx, call, searchOpts, toolSchemas, streamer.citations, streamer.toolCalls,
				func(ctx context.Context, call toolCall) (string, error) {
					return streamer.executeTool(ctx, registry, call)
				},
				"", "",
			)
			dl.WriteToolExec(call.name, call.arguments, output, time.Since(tstart), "")
			if !mixedTurn {
				toolOutputs = append(toolOutputs, map[string]any{
					"type":    "function_call_output",
					"call_id": call.id,
					"output":  output,
				})
			}
		}
		// Mixed turn: see the mixed-turn contract on runToolLoop.
		if mixedTurn {
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
