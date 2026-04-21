package toolruntime

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

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
	session, ok := registry.sessionFor(call.name)
	if !ok {
		return "", fmt.Errorf("no MCP session registered for tool %q", call.name)
	}
	switch call.name {
	case "search":
		id := "ws_" + uuid.NewString()
		action := map[string]any{"type": "search", "query": stringValue(call.arguments["query"])}
		outputIndex := s.openWebSearchCallItem(id, action)
		s.emitWebSearchCallPhase("response.web_search_call.in_progress", id, outputIndex)
		s.emitWebSearchCallPhase("response.web_search_call.searching", id, outputIndex)
		output, err := callTool(ctx, session, call.name, call.arguments, s.citations)
		if err != nil {
			reason := publicToolErrorReason(call.name, err)
			status := "failed"
			if reason == blockedToolErrorReason {
				status = "blocked"
			}
			s.closeWebSearchCallItem(id, outputIndex, action, status, reason)
			return "", err
		}
		s.emitWebSearchCallPhase("response.web_search_call.completed", id, outputIndex)
		s.closeWebSearchCallItem(id, outputIndex, action, "completed", "")
		return output, nil
	case "fetch":
		urls := fetchArgumentURLs(call.arguments)
		if len(urls) == 0 {
			return callTool(ctx, session, call.name, call.arguments, s.citations)
		}
		fetchIDs := make([]string, len(urls))
		fetchIndices := make([]int, len(urls))
		fetchActions := make([]map[string]any, len(urls))
		for i, url := range urls {
			fetchIDs[i] = "ws_" + uuid.NewString()
			fetchActions[i] = map[string]any{"type": "open_page", "url": url}
			fetchIndices[i] = s.openWebSearchCallItem(fetchIDs[i], fetchActions[i])
			s.emitWebSearchCallPhase("response.web_search_call.in_progress", fetchIDs[i], fetchIndices[i])
		}
		output, err := callTool(ctx, session, call.name, call.arguments, s.citations)
		if err != nil {
			reason := publicToolErrorReason(call.name, err)
			status := "failed"
			if reason == blockedToolErrorReason {
				status = "blocked"
			}
			for i := range urls {
				s.closeWebSearchCallItem(fetchIDs[i], fetchIndices[i], fetchActions[i], status, reason)
			}
			return "", err
		}
		for i := range urls {
			s.emitWebSearchCallPhase("response.web_search_call.completed", fetchIDs[i], fetchIndices[i])
			s.closeWebSearchCallItem(fetchIDs[i], fetchIndices[i], fetchActions[i], "completed", "")
		}
		return output, nil
	default:
		return callTool(ctx, session, call.name, call.arguments, s.citations)
	}
}

// openWebSearchCallItem emits response.output_item.added for a
// router-owned web_search_call item and returns the output_index the
// client will see for it. The item is NOT recorded into s.finalOutput
// because attachResponsesCitations rebuilds the terminal snapshot from
// citations.toolCalls; the live event here is solely for the client's
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

// emitWebSearchCallPhase emits a progress envelope for a web_search_call
// item in the shape OpenAI's spec defines: {item_id, output_index,
// sequence_number, type}. Only response.web_search_call.{in_progress,
// searching, completed} are spec-defined phase events; failures surface
// solely through the terminal response.output_item.done envelope.
func (s *responsesStreamer) emitWebSearchCallPhase(eventType, itemID string, outputIndex int) {
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
	// Build the final `output` array. Run it through attachResponsesCitations
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
	// attachResponsesCitations injects here. No tinfoil-event marker
	// channel rides on this path -- the Responses stream is always
	// fully OpenAI-spec-conformant.
	attachResponsesCitations(completedBody, s.citations, s.includeActionSources)
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
