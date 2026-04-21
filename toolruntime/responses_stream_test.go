package toolruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newTestResponsesStreamer builds a responsesStreamer backed by an
// httptest recorder, pre-marked as having written headers so tests can
// exercise event emission without driving a real upstream request.
// The router-owned tools are pre-seeded with `search` and `fetch` so
// tests exercising tool-call interception see the production-time
// classification without having to wire up the full Handle() flow.
func newTestResponsesStreamer(t *testing.T) (*responsesStreamer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	streamer := &responsesStreamer{
		streamBase: streamBase{
			w:                     rec,
			flusher:               rec,
			usageMetricsRequested: true,
			citations:             &citationState{nextIndex: 1},
			usageTotals:           &usageAccumulator{},
			model:                 "gpt-oss-120b",
			headersWritten:        true,
		},
		emitters:              map[itemContentKey]*citationEmitter{},
		functionCallArguments: map[int]*strings.Builder{},
		responseID:            "resp_test",
		createdAt:             1700000000,
		outputIndexMap:        map[int]int{},
		suppressedItems:       map[int]struct{}{},
		ownedTools: map[string]struct{}{
			"search": {},
			"fetch":  {},
		},
	}
	return streamer, rec
}

func TestResponsesStreamerPumpForwardsTextAndCapturesToolCalls(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	frames := []string{
		"event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_upstream","model":"gpt-oss-120b","created_at":1700000001}}`,
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
		"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"hello "}`,
		"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"world"}`,
		"event: response.output_text.done\n" + `data: {"type":"response.output_text.done","output_index":0,"item_id":"msg_1","content_index":0,"text":"hello world"}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_1","arguments":"{\"query\":\"go\"}"}}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_1","arguments":"{\"query\":\"go\"}"}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_upstream","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	reader := newSSEReader(strings.NewReader(upstream))

	result, err := streamer.pumpUpstream(reader, true)
	if err != nil {
		t.Fatalf("pumpUpstream returned error: %v", err)
	}
	if streamer.responseID != "resp_upstream" {
		t.Fatalf("expected upstream id captured, got %q", streamer.responseID)
	}
	if len(result.toolCalls) != 1 || result.toolCalls[0].name != "search" {
		t.Fatalf("expected search tool call, got %#v", result.toolCalls)
	}
	if got := result.toolCalls[0].arguments["query"]; got != "go" {
		t.Fatalf("expected arguments to parse, got %#v", got)
	}

	body := rec.Body.String()
	for _, fragment := range []string{
		"event: response.created",
		`"id":"resp_upstream"`,
		"event: response.output_text.delta",
		`"delta":"hello "`,
		"event: response.output_item.done",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("expected body to contain %q, got %s", fragment, body)
		}
	}
	if strings.Contains(body, `"type":"function_call"`) {
		t.Fatalf("function_call output items must be suppressed, got %s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("response.completed must be synthesized by finalize, not forwarded: %s", body)
	}
}

func TestResponsesStreamerFinalizeEmitsCompletedEvent(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	streamer.finalOutput = []any{
		map[string]any{"id": "msg_1", "type": "message"},
	}
	streamer.usageTotals.Add(&upstreamJSONResponse{body: map[string]any{
		"usage": map[string]any{
			"input_tokens":  float64(7),
			"output_tokens": float64(11),
			"total_tokens":  float64(18),
		},
	}})

	req := httptest.NewRequest("POST", "/v1/responses", nil)
	if err := streamer.finalize(req, nil, "gpt-oss-120b", responsesIterationResult{}); err != nil {
		t.Fatalf("finalize returned error: %v", err)
	}

	body := rec.Body.String()
	for _, fragment := range []string{
		"event: response.completed",
		`"status":"completed"`,
		`"output_tokens":11`,
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("expected body to contain %q, got %s", fragment, body)
		}
	}
}

func TestResponsesStreamerAnnotationsResolveInline(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	streamer.citations.sources = []citationSource{
		{index: 1, url: "https://example.com/a", title: "A"},
	}
	streamer.outputIndexMap[0] = 0

	streamer.handleOutputTextDelta(map[string]any{
		"type":          "response.output_text.delta",
		"output_index":  float64(0),
		"item_id":       "msg_1",
		"content_index": float64(0),
		"delta":         "See [A](https://example.com/a) for details.",
	})
	streamer.handleOutputTextDone(map[string]any{
		"type":          "response.output_text.done",
		"output_index":  float64(0),
		"item_id":       "msg_1",
		"content_index": float64(0),
	})

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.output_text.annotation.added") {
		t.Fatalf("expected annotation event, got %s", body)
	}
	if !strings.Contains(body, `"url":"https://example.com/a"`) {
		t.Fatalf("expected annotation to reference citation url, got %s", body)
	}
}

func TestResponsesStreamerPumpSurfacesResponseFailed(t *testing.T) {
	streamer, _ := newTestResponsesStreamer(t)
	frames := []string{
		"event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_upstream"}}`,
		"event: response.failed\n" + `data: {"type":"response.failed","response":{"id":"resp_upstream","error":{"message":"tool server unreachable"},"usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	reader := newSSEReader(strings.NewReader(upstream))

	_, err := streamer.pumpUpstream(reader, true)
	if err == nil {
		t.Fatal("expected response.failed to surface as upstreamError, got nil")
	}
	upErr, ok := err.(*upstreamError)
	if !ok {
		t.Fatalf("expected *upstreamError, got %T", err)
	}
	if !strings.Contains(string(upErr.body), "tool server unreachable") {
		t.Fatalf("expected error body to carry reason, got %s", upErr.body)
	}
}

func TestResponsesStreamerOutputIndexMonotonicAcrossIterations(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)

	turn := func(itemID string) string {
		return strings.Join([]string{
			"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"` + itemID + `","type":"message"}}`,
			"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"` + itemID + `","type":"message"}}`,
			"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_upstream"}}`,
		}, "\n\n") + "\n\n"
	}

	// First iteration fresh; second iteration also starts at upstream
	// output_index 0 but client should see index 1 because the streamer
	// keeps its own monotonic counter.
	streamer.resetPerIterationState()
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn("msg_1"))), true); err != nil {
		t.Fatalf("first pump err: %v", err)
	}
	streamer.resetPerIterationState()
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn("msg_2"))), false); err != nil {
		t.Fatalf("second pump err: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"item":{"id":"msg_1","type":"message"},"output_index":0`) {
		t.Fatalf("expected first item at output_index 0, got %s", body)
	}
	if !strings.Contains(body, `"item":{"id":"msg_2","type":"message"},"output_index":1`) {
		t.Fatalf("expected second item at output_index 1, got %s", body)
	}
}

// TestResponsesStreamerForwardsClientOwnedFunctionCall pins the contract
// that function_calls naming a tool the router does not own (i.e., a
// client-declared tool) are forwarded through the live stream AND echoed
// in response.completed.output so callers see the tool request either
// via live events or the terminal snapshot.
func TestResponsesStreamerForwardsClientOwnedFunctionCall(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	frames := []string{
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"client_lookup","call_id":"call_x","arguments":"{\"id\":1}"}}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"client_lookup","call_id":"call_x","arguments":"{\"id\":1}"}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_upstream"}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	result, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)), true)
	if err != nil {
		t.Fatalf("pumpUpstream returned error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"name":"client_lookup"`) {
		t.Fatalf("expected client_lookup function_call to be forwarded live, got %s", body)
	}
	if !strings.Contains(body, "event: response.output_item.done") {
		t.Fatalf("expected output_item.done to be forwarded live, got %s", body)
	}
	// The caller must still be told about the client-owned tool call so
	// the outer loop treats it as a client tool and finalize emits the
	// corresponding entry in response.completed.output.
	if len(result.toolCalls) != 1 || result.toolCalls[0].name != "client_lookup" {
		t.Fatalf("expected client_lookup tool call in iteration result, got %#v", result.toolCalls)
	}
	if len(streamer.finalOutput) != 1 {
		t.Fatalf("expected finalOutput to contain client function_call, got %#v", streamer.finalOutput)
	}
}

// TestResponsesStreamerFinalizeAttachesAnnotationsToCompletedOutput pins
// the parity contract with the non-streaming path: the terminal
// response.completed event echoes normalized text AND flat url_citation
// annotations, not the raw upstream item. Clients that consume only the
// final snapshot must see the same answer clients that replay the live
// stream see.
func TestResponsesStreamerFinalizeAttachesAnnotationsToCompletedOutput(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	streamer.citations.sources = []citationSource{
		{index: 1, url: "https://example.com/a", title: "A"},
	}
	// Simulate an item captured at output_item.done carrying a fullwidth
	// bracketed citation link that the live emitter would have normalized
	// when it streamed the delta. finalize must apply the same
	// normalization and attach the annotation on the terminal snapshot.
	streamer.finalOutput = []any{
		map[string]any{
			"id":   "msg_1",
			"type": "message",
			"content": []any{
				map[string]any{
					"type": "output_text",
					"text": "See \u3010A\u3011(https://example.com/a).",
				},
			},
		},
	}
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	if err := streamer.finalize(req, nil, "gpt-oss-120b", responsesIterationResult{}); err != nil {
		t.Fatalf("finalize returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"text":"See [A](https://example.com/a)."`) {
		t.Fatalf("expected normalized ASCII markdown in response.completed, got %s", body)
	}
	if !strings.Contains(body, `"annotations":[{`) || !strings.Contains(body, `"url":"https://example.com/a"`) {
		t.Fatalf("expected flat url_citation annotations on response.completed, got %s", body)
	}
}

// TestResponsesStreamerPumpSurfacesAbruptDisconnect pins that a stream
// ending before any terminal response.* event is treated as an upstream
// disconnect error rather than silently interpreted as success.
func TestResponsesStreamerPumpSurfacesAbruptDisconnect(t *testing.T) {
	streamer, _ := newTestResponsesStreamer(t)
	frames := []string{
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	_, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)), true)
	if err == nil {
		t.Fatal("expected abrupt EOF without terminal event to surface as error")
	}
	upErr, ok := err.(*upstreamError)
	if !ok {
		t.Fatalf("expected *upstreamError, got %T", err)
	}
	if !strings.Contains(string(upErr.body), "terminal response event") {
		t.Fatalf("expected disconnect-specific error body, got %s", upErr.body)
	}
}

// TestResponsesStreamerTerminateBillingUsesMultiTurnTotals pins that a
// failure after several successful iterations bills the aggregate token
// counts, not just whatever the failing turn happened to surface.
func TestResponsesStreamerTerminateBillingUsesMultiTurnTotals(t *testing.T) {
	streamer, _ := newTestResponsesStreamer(t)
	// Turn 1 spent tokens and succeeded (captured into usageTotals).
	streamer.usageTotals.Add(&upstreamJSONResponse{body: map[string]any{
		"usage": map[string]any{
			"input_tokens":  float64(10),
			"output_tokens": float64(20),
			"total_tokens":  float64(30),
		},
	}})
	// Turn 2 partially streamed 1 output token before failing; the
	// streamer recorded just that partial usage as aggregatedUsage.
	streamer.aggregatedUsage = map[string]any{
		"input_tokens":  float64(5),
		"output_tokens": float64(1),
		"total_tokens":  float64(6),
	}
	streamer.usageTotals.Add(&upstreamJSONResponse{body: map[string]any{
		"usage": streamer.aggregatedUsage,
	}})
	billing := streamer.totalsBillingUsage()
	if got := int(billing["input_tokens"].(int)); got != 15 {
		t.Fatalf("expected aggregated input_tokens=15, got %d", got)
	}
	if got := int(billing["output_tokens"].(int)); got != 21 {
		t.Fatalf("expected aggregated output_tokens=21, got %d", got)
	}
	if got := int(billing["total_tokens"].(int)); got != 36 {
		t.Fatalf("expected aggregated total_tokens=36, got %d", got)
	}
}

// TestResponsesStreamerAdoptsUpstreamCreatedAt pins that the synthesized
// response.created event carries upstream's own created_at and id, not a
// router wall-clock timestamp or router-minted id. This would have been
// silently masked by the test helper pre-seeding identity in previous
// iterations; the reset below is required to exercise the production
// captureIdentity path the real runIteration() runs through.
func TestResponsesStreamerAdoptsUpstreamCreatedAt(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	streamer.responseID = ""
	streamer.createdAt = 0
	streamer.model = ""
	streamer.responseCreatedEmitted = false

	frames := []string{
		"event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_real","created_at":1700000123,"model":"gpt-oss-120b-0416"}}`,
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_real"}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)), true); err != nil {
		t.Fatalf("pump err: %v", err)
	}
	if streamer.responseID != "resp_real" {
		t.Fatalf("expected upstream response id, got %q", streamer.responseID)
	}
	if streamer.createdAt != 1700000123 {
		t.Fatalf("expected upstream created_at, got %d", streamer.createdAt)
	}
	if streamer.model != "gpt-oss-120b-0416" {
		t.Fatalf("expected upstream model, got %q", streamer.model)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"created_at":1700000123`) {
		t.Fatalf("expected synthesized response.created to carry upstream created_at, got %s", body)
	}
	if !strings.Contains(body, `"id":"resp_real"`) {
		t.Fatalf("expected synthesized response.created to carry upstream id, got %s", body)
	}
	if strings.Contains(body, `"id":"resp_`) && strings.Contains(body, `"id":"resp_`+"") {
		// sanity: the router-minted id prefix is "resp_" + uuid; a real
		// upstream id looks the same pattern but the test hooks strictly
		// against the string "resp_real".
	}
}

// TestResponsesStreamerClientOwnedFunctionCallReassemblesArguments pins
// that client-owned function_call items whose arguments arrive only
// through `response.function_call_arguments.delta` frames (and land on
// `response.output_item.done` with an empty arguments string) still end
// up with a reassembled arguments payload in the live forwarded event,
// in finalOutput, and in result.toolCalls.
func TestResponsesStreamerClientOwnedFunctionCallReassemblesArguments(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	frames := []string{
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"client_lookup","call_id":"call_x","arguments":""}}`,
		"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"id\":"}`,
		"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"42}"}`,
		"event: response.function_call_arguments.done\n" + `data: {"type":"response.function_call_arguments.done","output_index":0}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"client_lookup","call_id":"call_x","arguments":""}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_upstream"}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	result, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)), true)
	if err != nil {
		t.Fatalf("pump err: %v", err)
	}
	if len(result.toolCalls) != 1 {
		t.Fatalf("expected 1 client tool call, got %d", len(result.toolCalls))
	}
	if got := result.toolCalls[0].arguments["id"]; got != float64(42) {
		t.Fatalf("expected reassembled arguments id=42, got %#v", got)
	}
	if len(streamer.finalOutput) != 1 {
		t.Fatalf("expected 1 final output item, got %d", len(streamer.finalOutput))
	}
	finalItem, _ := streamer.finalOutput[0].(map[string]any)
	if got := stringValue(finalItem["arguments"]); got != `{"id":42}` {
		t.Fatalf("expected final item arguments reassembled, got %q", got)
	}
	body := rec.Body.String()
	// The forwarded output_item.done event must also carry the
	// reassembled arguments string.
	if !strings.Contains(body, `"arguments":"{\"id\":42}"`) {
		t.Fatalf("expected forwarded item.done to carry reassembled arguments, got %s", body)
	}
}

// TestResponsesStreamerAnnotationIndexMonotonic pins that
// `annotation_index` on response.output_text.annotation.added is
// monotonically increasing across every push/flush cycle for a given
// (item_id, content_index) pair, not reset per batch.
func TestResponsesStreamerAnnotationIndexMonotonic(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)
	streamer.citations.sources = []citationSource{
		{index: 1, url: "https://example.com/a", title: "A"},
		{index: 2, url: "https://example.com/b", title: "B"},
	}
	streamer.outputIndexMap[0] = 0

	// Two separate deltas, each resolving one citation; the second must
	// be annotation_index:1, not annotation_index:0 again.
	streamer.handleOutputTextDelta(map[string]any{
		"type":          "response.output_text.delta",
		"output_index":  float64(0),
		"item_id":       "msg_1",
		"content_index": float64(0),
		"delta":         "First [A](https://example.com/a). ",
	})
	streamer.handleOutputTextDelta(map[string]any{
		"type":          "response.output_text.delta",
		"output_index":  float64(0),
		"item_id":       "msg_1",
		"content_index": float64(0),
		"delta":         "Second [B](https://example.com/b).",
	})
	streamer.handleOutputTextDone(map[string]any{
		"type":          "response.output_text.done",
		"output_index":  float64(0),
		"item_id":       "msg_1",
		"content_index": float64(0),
	})

	body := rec.Body.String()
	if !strings.Contains(body, `"annotation_index":0`) {
		t.Fatalf("expected first annotation at index 0, got %s", body)
	}
	if !strings.Contains(body, `"annotation_index":1`) {
		t.Fatalf("expected second annotation at index 1, got %s", body)
	}
	if strings.Count(body, `"annotation_index":0`) != 1 {
		t.Fatalf("expected exactly one annotation_index:0 event, got %d in %s", strings.Count(body, `"annotation_index":0`), body)
	}
}

func TestResponsesStreamerFunctionCallArgumentsReassembly(t *testing.T) {
	streamer, _ := newTestResponsesStreamer(t)
	frames := []string{
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_1","arguments":""}}`,
		"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"query\":\""}`,
		"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"go\"}"}`,
		"event: response.function_call_arguments.done\n" + `data: {"type":"response.function_call_arguments.done","output_index":0}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_1","arguments":""}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_upstream"}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	reader := newSSEReader(strings.NewReader(upstream))

	result, err := streamer.pumpUpstream(reader, true)
	if err != nil {
		t.Fatalf("pumpUpstream returned error: %v", err)
	}
	if len(result.toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.toolCalls))
	}
	if got := result.toolCalls[0].arguments["query"]; got != "go" {
		t.Fatalf("expected reassembled arguments to parse, got %#v", got)
	}
}

// TestResponsesStreamerRouterOwnedArgsSplicedBackIntoItem pins that for
// router-owned function_call items that ship arguments only via delta
// frames, the reassembled JSON is spliced back into item["arguments"]
// before the item lands in result.outputItems. The outer loop replays
// result.outputItems into the next /v1/responses turn's input, so if the
// splice were missing the model would see its own prior tool call with
// an empty arguments string -- which is exactly the bug this guards.
func TestResponsesStreamerRouterOwnedArgsSplicedBackIntoItem(t *testing.T) {
	streamer, _ := newTestResponsesStreamer(t)
	frames := []string{
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_1","arguments":""}}`,
		"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"query\":"}`,
		"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"go\"}"}`,
		"event: response.function_call_arguments.done\n" + `data: {"type":"response.function_call_arguments.done","output_index":0}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"search","call_id":"call_1","arguments":""}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_upstream"}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	result, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)), true)
	if err != nil {
		t.Fatalf("pump err: %v", err)
	}
	if len(result.outputItems) != 1 {
		t.Fatalf("expected 1 outputItem, got %d", len(result.outputItems))
	}
	item, _ := result.outputItems[0].(map[string]any)
	if item == nil {
		t.Fatalf("expected map item, got %#v", result.outputItems[0])
	}
	if got := stringValue(item["arguments"]); got != `{"query":"go"}` {
		t.Fatalf("expected arguments spliced back into item, got %q", got)
	}
}

// TestResponsesStreamerFinalOutputResetsPerIteration pins that the
// terminal response.completed.output snapshot reflects only the items
// emitted during the final iteration, matching the non-streaming path's
// semantics. Without the reset at the top of runIteration, finalOutput
// would accumulate items across tool turns and ship stale pre-tool
// reasoning or message items in the terminal event.
func TestResponsesStreamerFinalOutputResetsPerIteration(t *testing.T) {
	streamer, _ := newTestResponsesStreamer(t)
	// Simulate an earlier iteration having left items in finalOutput.
	streamer.finalOutput = []any{map[string]any{"id": "stale", "type": "message"}}

	frames := []string{
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_final","type":"message"}}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_final","type":"message"}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_upstream"}}`,
	}
	// runIteration calls resetPerIterationState before pumpUpstream;
	// exercise the same production method here so this test pins the
	// actual reset path rather than a parallel inline reset that could
	// drift if the scope boundary ever changes.
	streamer.resetPerIterationState()
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(strings.Join(frames, "\n\n")+"\n\n")), false); err != nil {
		t.Fatalf("pump err: %v", err)
	}
	if len(streamer.finalOutput) != 1 {
		t.Fatalf("expected finalOutput to contain only final-turn items, got %d", len(streamer.finalOutput))
	}
	item, _ := streamer.finalOutput[0].(map[string]any)
	if stringValue(item["id"]) != "msg_final" {
		t.Fatalf("expected finalOutput item id msg_final, got %q", stringValue(item["id"]))
	}
}

// TestResponsesStreamerMalformedJSONFailsStream pins that a malformed
// upstream SSE frame terminates the stream with an upstreamError
// instead of silently dropping the chunk -- matching the non-streaming
// path's hard-fail contract. Tolerating malformed frames would let a
// stream succeed while silently losing content, tool calls, or usage.
func TestResponsesStreamerMalformedJSONFailsStream(t *testing.T) {
	streamer, _ := newTestResponsesStreamer(t)
	upstream := strings.Join([]string{
		"event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_upstream"`,
	}, "\n\n") + "\n\n"
	_, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)), true)
	if err == nil {
		t.Fatalf("expected error on malformed JSON, got nil")
	}
	upErr, ok := err.(*upstreamError)
	if !ok {
		t.Fatalf("expected upstreamError, got %T", err)
	}
	if !strings.Contains(string(upErr.body), "malformed SSE JSON") {
		t.Fatalf("expected malformed-JSON message in body, got %s", string(upErr.body))
	}
}

// TestResponsesStreamerPumpAbortsOnClientDisconnect pins that once the
// SSE write helper latches a client-disconnect error, pumpUpstream
// stops reading upstream frames and surfaces the write error so the
// outer loop can abort before running another tool call.
func TestResponsesStreamerPumpAbortsOnClientDisconnect(t *testing.T) {
	w := &failingFlushWriter{}
	streamer := &responsesStreamer{
		streamBase: streamBase{
			w:              w,
			flusher:        w,
			citations:      &citationState{nextIndex: 1},
			usageTotals:    &usageAccumulator{},
			model:          "gpt-oss-120b",
			headersWritten: true,
		},
		emitters:              map[itemContentKey]*citationEmitter{},
		annotationCounts:      map[itemContentKey]int{},
		functionCallArguments: map[int]*strings.Builder{},
		outputIndexMap:        map[int]int{},
		suppressedItems:       map[int]struct{}{},
		responseID:            "resp_test",
		createdAt:             1,
		ownedTools:            map[string]struct{}{"search": {}},
	}

	frames := []string{
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
		"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"hello"}`,
		"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"world"}`,
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_test"}}`,
	}
	upstream := strings.Join(frames, "\n\n") + "\n\n"
	_, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)), true)
	if err == nil {
		t.Fatalf("expected pump to surface write error on client disconnect, got nil")
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe, got %v", err)
	}
	if streamer.writeErr == nil {
		t.Fatalf("expected streamer.writeErr to be latched")
	}
}

// responsesIterationTurn builds an SSE frame sequence for one upstream
// iteration of a /responses stream, used by the multi-iteration tests
// below to stitch together 2- and 3-turn scenarios. Callers pass the
// items the upstream model emits on this iteration in order; the helper
// prepends response.created and appends response.completed so the pump
// sees a well-formed turn. Each item is either a `message` (assistant
// text) or a `function_call` (tool call the router will either execute
// or forward). Upstream resets output_index to 0 at the top of every
// turn, matching real upstream behavior.
func responsesIterationTurn(responseID string, items []any) string {
	var frames []string
	frames = append(frames,
		`event: response.created`+"\n"+
			`data: {"type":"response.created","response":{"id":"`+responseID+`","model":"gpt-oss-120b","created_at":1700000001}}`,
	)
	for idx, raw := range items {
		item, _ := raw.(map[string]any)
		itemBytes, _ := json.Marshal(item)
		frames = append(frames,
			`event: response.output_item.added`+"\n"+
				`data: {"type":"response.output_item.added","output_index":`+stringFromInt(idx)+`,"item":`+string(itemBytes)+`}`,
		)
		if stringValue(item["type"]) == "message" {
			text := stringValue(item["text"])
			if text == "" {
				text = "hi"
			}
			frames = append(frames,
				`event: response.output_text.delta`+"\n"+
					`data: {"type":"response.output_text.delta","output_index":`+stringFromInt(idx)+`,"item_id":"`+stringValue(item["id"])+`","content_index":0,"delta":`+jsonString(text)+`}`,
				`event: response.output_text.done`+"\n"+
					`data: {"type":"response.output_text.done","output_index":`+stringFromInt(idx)+`,"item_id":"`+stringValue(item["id"])+`","content_index":0,"text":`+jsonString(text)+`}`,
			)
		}
		frames = append(frames,
			`event: response.output_item.done`+"\n"+
				`data: {"type":"response.output_item.done","output_index":`+stringFromInt(idx)+`,"item":`+string(itemBytes)+`}`,
		)
	}
	frames = append(frames,
		`event: response.completed`+"\n"+
			`data: {"type":"response.completed","response":{"id":"`+responseID+`","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	)
	return strings.Join(frames, "\n\n") + "\n\n"
}

func stringFromInt(i int) string { return fmt.Sprintf("%d", i) }

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestResponsesStreamerTwoIterationStitching pins the N=2 multi-turn
// invariants: the client sees exactly one response.created, zero
// per-iteration response.completed frames (they are all swallowed; the
// finalize step emits the router-aggregated one), the router-owned
// function_call from iter0 is fully suppressed on the client stream,
// and the final assistant message from iter1 flows through unchanged.
func TestResponsesStreamerTwoIterationStitching(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)

	// Iteration 0: upstream emits a function_call naming a router-owned
	// tool. pump captures it, suppresses it from the wire, and the
	// outer driver would run it and re-pump.
	turn0 := responsesIterationTurn("resp_upstream", []any{
		map[string]any{"id": "fc_1", "type": "function_call", "name": "search", "call_id": "call_a", "arguments": `{"query":"go"}`},
	})
	streamer.resetPerIterationState()
	result0, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn0)), true)
	if err != nil {
		t.Fatalf("iter0 pump err: %v", err)
	}
	if len(result0.toolCalls) != 1 || result0.toolCalls[0].name != "search" {
		t.Fatalf("iter0 expected 1 search tool call, got %+v", result0.toolCalls)
	}

	// Iteration 1: upstream emits the final assistant message.
	turn1 := responsesIterationTurn("resp_upstream", []any{
		map[string]any{"id": "msg_final", "type": "message", "text": "done"},
	})
	streamer.resetPerIterationState()
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn1)), false); err != nil {
		t.Fatalf("iter1 pump err: %v", err)
	}

	body := rec.Body.String()
	if n := strings.Count(body, "event: response.created"); n != 1 {
		t.Fatalf("expected exactly 1 response.created frame across 2 iterations, got %d", n)
	}
	if n := strings.Count(body, "event: response.completed"); n != 0 {
		t.Fatalf("response.completed must be swallowed by pump on all iterations (finalize emits one); got %d", n)
	}
	// The router-owned function_call from iter0 must be fully
	// suppressed: no function_call output item, no fc_1 id leak.
	if strings.Contains(body, `"id":"fc_1"`) {
		t.Fatalf("router-owned function_call from iter0 must be suppressed, got %s", body)
	}
	if strings.Contains(body, `"function_call"`) {
		t.Fatalf("router-owned function_call items must not surface on wire, got %s", body)
	}
	// The final message must land at output_index 0 on the client
	// stream: the suppressed function_call consumed no client-facing
	// slot, so iter1's first item maps to client output_index 0.
	if !strings.Contains(body, `"item":{"id":"msg_final","text":"done","type":"message"},"output_index":0`) {
		t.Fatalf("expected final message at client output_index 0 (iter0 suppressed), got %s", body)
	}
}

// TestResponsesStreamerThreeIterationStitching pins the N=3 path: two
// consecutive router-owned function_calls before a final message. Both
// tool-call turns must be fully suppressed from the client stream, the
// final message must flow through normally, and there must be exactly
// one response.created across the whole logical stream.
func TestResponsesStreamerThreeIterationStitching(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)

	turns := []string{
		responsesIterationTurn("resp_upstream", []any{
			map[string]any{"id": "fc_1", "type": "function_call", "name": "search", "call_id": "call_a", "arguments": `{"query":"q1"}`},
		}),
		responsesIterationTurn("resp_upstream", []any{
			map[string]any{"id": "fc_2", "type": "function_call", "name": "search", "call_id": "call_b", "arguments": `{"query":"q2"}`},
		}),
		responsesIterationTurn("resp_upstream", []any{
			map[string]any{"id": "msg_final", "type": "message", "text": "answer"},
		}),
	}

	for i, turn := range turns {
		streamer.resetPerIterationState()
		if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn)), i == 0); err != nil {
			t.Fatalf("iter%d pump err: %v", i, err)
		}
	}

	body := rec.Body.String()
	if n := strings.Count(body, "event: response.created"); n != 1 {
		t.Fatalf("expected 1 response.created across 3 iterations, got %d", n)
	}
	if n := strings.Count(body, "event: response.completed"); n != 0 {
		t.Fatalf("response.completed must be swallowed by pump, got %d", n)
	}
	if strings.Contains(body, `"function_call"`) {
		t.Fatalf("router-owned function_calls must not reach client across 3 iterations, got %s", body)
	}
	// The final message is the only client-visible output item across
	// the three turns, so it lands at output_index 0.
	if !strings.Contains(body, `"item":{"id":"msg_final","text":"answer","type":"message"},"output_index":0`) {
		t.Fatalf("expected final message at client output_index 0, got %s", body)
	}
}

// TestResponsesStreamerOutputIndexAdvancesForClientOwnedToolCalls pins
// that a CLIENT-owned function_call (upstream asks the caller to run a
// tool the router does not intercept) IS forwarded, and its forwarded
// output item consumes a client-facing output_index slot. Combined
// with a following final message, this proves the counter advances
// across items that actually reach the client -- even across upstream
// iteration boundaries that reset upstream's own counter.
func TestResponsesStreamerOutputIndexAdvancesForClientOwnedToolCalls(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)

	// Iteration 0: upstream emits a function_call for a tool the router
	// does not own. It must be forwarded to the client, advancing the
	// persistent output_index counter.
	turn0 := responsesIterationTurn("resp_upstream", []any{
		map[string]any{"id": "fc_client", "type": "function_call", "name": "client_lookup", "call_id": "call_c", "arguments": `{"id":1}`},
	})
	streamer.resetPerIterationState()
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn0)), true); err != nil {
		t.Fatalf("iter0 pump err: %v", err)
	}

	// Iteration 1: upstream emits the final message. Upstream resets
	// its output_index to 0, but the streamer's persistent counter
	// must assign client output_index 1 to this item because iter0's
	// client_lookup already consumed slot 0.
	turn1 := responsesIterationTurn("resp_upstream", []any{
		map[string]any{"id": "msg_final", "type": "message", "text": "done"},
	})
	streamer.resetPerIterationState()
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn1)), false); err != nil {
		t.Fatalf("iter1 pump err: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"id":"fc_client"`) {
		t.Fatalf("client-owned function_call must be forwarded, got %s", body)
	}
	if !strings.Contains(body, `"item":{"id":"msg_final","text":"done","type":"message"},"output_index":1`) {
		t.Fatalf("expected final message at client output_index 1 (iter0 consumed slot 0), got %s", body)
	}
}

// TestResponsesStreamerSequenceNumberMonotonicAcrossIterations pins the
// persistent sequence_number counter: upstream resets it per turn, but
// every client-facing frame must carry a strictly increasing
// sequence_number across the whole logical stream. This is the
// invariant strict OpenAI SDKs rely on to order events when replaying
// a captured stream.
func TestResponsesStreamerSequenceNumberMonotonicAcrossIterations(t *testing.T) {
	streamer, rec := newTestResponsesStreamer(t)

	turns := []string{
		responsesIterationTurn("resp_upstream", []any{
			map[string]any{"id": "fc_1", "type": "function_call", "name": "search", "call_id": "call_a", "arguments": `{"query":"q1"}`},
		}),
		responsesIterationTurn("resp_upstream", []any{
			map[string]any{"id": "msg_final", "type": "message", "text": "answer"},
		}),
	}
	for i, turn := range turns {
		streamer.resetPerIterationState()
		if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn)), i == 0); err != nil {
			t.Fatalf("iter%d pump err: %v", i, err)
		}
	}

	// Extract sequence numbers from every `data:` frame and confirm the
	// sequence is strictly monotonically increasing starting from 0.
	var seqs []int
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &decoded); err != nil {
			t.Fatalf("invalid JSON frame: %v", err)
		}
		if seq, ok := decoded["sequence_number"].(float64); ok {
			seqs = append(seqs, int(seq))
		}
	}
	if len(seqs) < 3 {
		t.Fatalf("expected multiple sequenced frames, got %d", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("sequence_number not strictly monotonic (upstream per-iteration reset leaked): seqs=%v", seqs)
		}
	}
}

// TestResponsesStreamerFallbacksLogOnceWhenUpstreamOmitsIdentity pins
// that the response id and created_at fallbacks emit a single log line
// each when they actually fire. vLLM's /responses implementation always
// emits both on response.created; a hit on this fallback signals an
// upstream regression that operators need to see.
func TestResponsesStreamerFallbacksLogOnceWhenUpstreamOmitsIdentity(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	streamer, _ := newTestResponsesStreamer(t)
	streamer.responseID = ""
	streamer.createdAt = 0

	if got := streamer.streamResponseID(); !strings.HasPrefix(got, "resp_") {
		t.Fatalf("streamResponseID fallback should be router-minted, got %q", got)
	}
	if got := streamer.streamCreatedAt(); got <= 0 {
		t.Fatalf("streamCreatedAt fallback should be a positive unix time, got %d", got)
	}
	_ = streamer.streamResponseID()
	_ = streamer.streamCreatedAt()

	logged := buf.String()
	if idHits := strings.Count(logged, "upstream omitted response id"); idHits != 1 {
		t.Fatalf("expected exactly one id-fallback log line, got %d in %q", idHits, logged)
	}
	if createdHits := strings.Count(logged, "upstream omitted response created_at"); createdHits != 1 {
		t.Fatalf("expected exactly one created_at-fallback log line, got %d in %q", createdHits, logged)
	}
}

// TestResponsesStreamerDoesNotLogWhenUpstreamProvidesIdentity pins the
// inverse: the happy path must produce zero log noise.
func TestResponsesStreamerDoesNotLogWhenUpstreamProvidesIdentity(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	streamer, _ := newTestResponsesStreamer(t)
	_ = streamer.streamResponseID()
	_ = streamer.streamCreatedAt()
	_ = streamer.streamResponseID()

	if logged := buf.String(); logged != "" {
		t.Fatalf("happy path must not log; got %q", logged)
	}
}
