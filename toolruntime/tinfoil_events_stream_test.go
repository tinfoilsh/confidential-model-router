package toolruntime

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestResponsesStreamerForSpecEvents is a minimal factory mirroring
// newTestResponsesStreamer from responses_stream_test.go, used by the
// web_search_call spec event tests on this file.
func newTestResponsesStreamerForSpecEvents(t *testing.T) (*responsesStreamer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	streamer := &responsesStreamer{
		w:                     rec,
		flusher:               rec,
		usageMetricsRequested: true,
		citations:             &citationState{nextIndex: 1},
		usageTotals:           &usageAccumulator{},
		emitters:              map[itemContentKey]*citationEmitter{},
		annotationCounts:      map[itemContentKey]int{},
		functionCallArguments: map[int]*strings.Builder{},
		ownedTools:            map[string]struct{}{"search": {}, "fetch": {}},
		responseID:            "resp_test",
		upstreamIDCaptured:    true,
		createdAt:             1700000000,
		model:                 "gpt-oss-120b",
		headersWritten:        true,
	}
	return streamer, rec
}

// TestChatStreamerEmitTinfoilEventMarkerWritesDeltaWhenEnabled pins the
// streaming-chat contract: a single marker becomes one chat.completion.chunk
// whose only delta field is `content`. Forgiving SDKs concatenate this
// into the assistant message as tagged text; opt-in clients strip it
// with a regex.
func TestChatStreamerEmitTinfoilEventMarkerWritesDeltaWhenEnabled(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	streamer.eventsEnabled = true

	streamer.emitTinfoilEventMarker("ws_1", "in_progress", map[string]any{"type": "search", "query": "q"}, "")

	body := rec.Body.String()
	// Pull exactly one `data:` frame, decode it, and assert the
	// marker round-trips intact via delta.content. This is stronger
	// than a substring match because json.Marshal HTML-escapes `<` and
	// `>` on the wire, and it also pins that the envelope is a proper
	// chat.completion.chunk so strict SDKs accept it.
	frame := firstSSEDataFrame(t, body)
	var chunk map[string]any
	if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
		t.Fatalf("chat stream frame must be valid JSON: %v (%q)", err, frame)
	}
	if chunk["object"] != "chat.completion.chunk" {
		t.Fatalf("marker frame must be a chat.completion.chunk, got object=%v", chunk["object"])
	}
	choice := chunk["choices"].([]any)[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	content := delta["content"].(string)
	if !strings.Contains(content, tinfoilEventOpenTag) || !strings.Contains(content, tinfoilEventCloseTag) {
		t.Fatalf("delta.content missing tinfoil-event tags: %q", content)
	}
}

// firstSSEDataFrame returns the JSON payload of the first `data:` line
// in an SSE body, failing the test if none is present. Centralizing
// this keeps streaming tests focused on semantics.
func firstSSEDataFrame(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: {") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("no SSE data frame found in body: %q", body)
	return ""
}

// TestChatStreamerEmitTinfoilEventMarkerIsNoOpWhenDisabled pins the
// opt-out path: without the header, the marker helper writes nothing
// to the wire, preserving a fully pristine OpenAI-spec stream.
func TestChatStreamerEmitTinfoilEventMarkerIsNoOpWhenDisabled(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	streamer.eventsEnabled = false

	streamer.emitTinfoilEventMarker("ws_1", "in_progress", map[string]any{"type": "search"}, "")

	if body := rec.Body.String(); body != "" {
		t.Fatalf("expected empty stream when events are disabled, got %q", body)
	}
}

// TestResponsesStreamerEmitsSpecWebSearchCallEvents pins the Responses
// streaming contract for router-owned web_search tool calls: the router
// MUST emit the spec-defined event sequence documented by OpenAI
// (output_item.added + web_search_call.in_progress + web_search_call.searching
// for search + output_item.done on terminal status). No tinfoil-event
// marker frames ride on this surface.
func TestResponsesStreamerEmitsSpecWebSearchCallEvents(t *testing.T) {
	streamer, rec := newTestResponsesStreamerForSpecEvents(t)

	action := map[string]any{"type": "search", "query": "q"}
	idx := streamer.openWebSearchCallItem("ws_1", action)
	streamer.emitWebSearchCallPhase("response.web_search_call.in_progress", "ws_1", idx)
	streamer.emitWebSearchCallPhase("response.web_search_call.searching", "ws_1", idx)
	streamer.emitWebSearchCallPhase("response.web_search_call.completed", "ws_1", idx)
	streamer.closeWebSearchCallItem("ws_1", idx, action, "completed", "")

	body := rec.Body.String()
	wantOrder := []string{
		"event: response.output_item.added",
		"event: response.web_search_call.in_progress",
		"event: response.web_search_call.searching",
		"event: response.web_search_call.completed",
		"event: response.output_item.done",
	}
	cursor := 0
	for _, want := range wantOrder {
		idx := strings.Index(body[cursor:], want)
		if idx < 0 {
			t.Fatalf("missing %q in response stream (or out of order): %q", want, body)
		}
		cursor += idx + len(want)
	}

	// No tinfoil-event marker frames should appear on the Responses
	// path. Substring scan is enough because the payload is never
	// emitted live on this surface.
	if strings.Contains(body, tinfoilEventOpenTag) {
		t.Fatalf("Responses stream must not contain tinfoil-event markers: %q", body)
	}

	// Inspect the terminal output_item.done frame: item must carry
	// status=completed and the original action.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("response stream frame must be valid JSON: %v (%q)", err, payload)
		}
		if decoded["type"] != "response.output_item.done" {
			continue
		}
		item, _ := decoded["item"].(map[string]any)
		if item == nil {
			t.Fatalf("response.output_item.done missing item: %q", payload)
		}
		if item["type"] != "web_search_call" {
			t.Fatalf("terminal item type = %v, want web_search_call", item["type"])
		}
		if item["status"] != "completed" {
			t.Fatalf("terminal item status = %v, want completed", item["status"])
		}
		if item["id"] != "ws_1" {
			t.Fatalf("terminal item id = %v, want ws_1", item["id"])
		}
	}
}

// TestResponsesStreamerWebSearchCallFailedOmitsCompletedEvent pins the
// failure path: the OpenAI spec only defines .in_progress, .searching,
// and .completed envelopes for web_search_call; failures surface solely
// through the terminal output_item.done with status=failed.
func TestResponsesStreamerWebSearchCallFailedOmitsCompletedEvent(t *testing.T) {
	streamer, rec := newTestResponsesStreamerForSpecEvents(t)

	action := map[string]any{"type": "search", "query": "q"}
	idx := streamer.openWebSearchCallItem("ws_1", action)
	streamer.emitWebSearchCallPhase("response.web_search_call.in_progress", "ws_1", idx)
	streamer.closeWebSearchCallItem("ws_1", idx, action, "failed", "upstream_timeout")

	body := rec.Body.String()
	if strings.Contains(body, "response.web_search_call.completed") {
		t.Fatalf("failed web_search_call must not emit .completed: %q", body)
	}
	if !strings.Contains(body, "response.output_item.done") {
		t.Fatalf("failed web_search_call must still emit terminal output_item.done: %q", body)
	}
	// Inspect the terminal frame's status.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("frame must be valid JSON: %v", err)
		}
		if decoded["type"] != "response.output_item.done" {
			continue
		}
		item := decoded["item"].(map[string]any)
		if item["status"] != "failed" {
			t.Fatalf("terminal item status = %v, want failed", item["status"])
		}
	}
}

// TestResponsesStreamerBlockedWebSearchCallCarriesTinfoilSidecar pins
// that a safety-filter block on a router-owned web_search tool call
// surfaces on the live stream as:
//   - envelope `status: "failed"` (spec-conformant; OpenAI's
//     web_search_call.status enum has no `blocked` slot)
//   - vendor-extension `_tinfoil: {status: "blocked", error: {code: ...}}`
//     carrying the unfiltered router signal for Tinfoil-aware clients
//
// Strict OpenAI SDKs ignore `_tinfoil`; clients that want the richer
// UX read it off the terminal `output_item.done.item` directly.
func TestResponsesStreamerBlockedWebSearchCallCarriesTinfoilSidecar(t *testing.T) {
	streamer, rec := newTestResponsesStreamerForSpecEvents(t)

	action := map[string]any{"type": "search", "query": "sensitive"}
	idx := streamer.openWebSearchCallItem("ws_1", action)
	streamer.emitWebSearchCallPhase("response.web_search_call.in_progress", "ws_1", idx)
	streamer.closeWebSearchCallItem("ws_1", idx, action, "blocked", blockedToolErrorReason)

	body := rec.Body.String()
	var terminalItem map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &decoded); err != nil {
			t.Fatalf("frame must be valid JSON: %v", err)
		}
		if decoded["type"] == "response.output_item.done" {
			terminalItem = decoded["item"].(map[string]any)
			break
		}
	}
	if terminalItem == nil {
		t.Fatalf("expected a terminal response.output_item.done frame: %q", body)
	}
	if terminalItem["status"] != "failed" {
		t.Fatalf("envelope status must collapse onto failed, got %v", terminalItem["status"])
	}
	sidecar, ok := terminalItem["_tinfoil"].(map[string]any)
	if !ok {
		t.Fatalf("blocked terminal frame must carry a _tinfoil sidecar, got %#v", terminalItem["_tinfoil"])
	}
	if sidecar["status"] != "blocked" {
		t.Fatalf("_tinfoil.status must be the unfiltered router status, got %v", sidecar["status"])
	}
	errObj, _ := sidecar["error"].(map[string]any)
	if errObj == nil || errObj["code"] != blockedToolErrorReason {
		t.Fatalf("_tinfoil.error.code = %v, want %q", errObj, blockedToolErrorReason)
	}
}

// TestResponsesStreamerSuccessfulWebSearchCallOmitsTinfoilSidecar pins
// that the happy-path terminal frame stays minimal: a successful search
// never carries a _tinfoil field on the item because the spec envelope
// fully describes the signal. This keeps bytes off the wire for the
// common case and keeps the JSON surface easy to audit.
func TestResponsesStreamerSuccessfulWebSearchCallOmitsTinfoilSidecar(t *testing.T) {
	streamer, rec := newTestResponsesStreamerForSpecEvents(t)

	action := map[string]any{"type": "search", "query": "q"}
	idx := streamer.openWebSearchCallItem("ws_1", action)
	streamer.closeWebSearchCallItem("ws_1", idx, action, "completed", "")

	body := rec.Body.String()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &decoded); err != nil {
			t.Fatalf("frame must be valid JSON: %v", err)
		}
		if decoded["type"] != "response.output_item.done" {
			continue
		}
		item := decoded["item"].(map[string]any)
		if _, present := item["_tinfoil"]; present {
			t.Fatalf("successful terminal frame must omit _tinfoil sidecar, got %#v", item)
		}
	}
}

// TestResponsesStreamerWebSearchCallOutputIndexIsMonotonic pins that
// every spec-event burst consumes exactly one output_index slot so
// subsequent real output items keep a monotonically increasing
// client-facing index.
func TestResponsesStreamerWebSearchCallOutputIndexIsMonotonic(t *testing.T) {
	streamer, _ := newTestResponsesStreamerForSpecEvents(t)
	before := streamer.outputIndex

	_ = streamer.openWebSearchCallItem("ws_1", map[string]any{"type": "search"})
	_ = streamer.openWebSearchCallItem("ws_2", map[string]any{"type": "search"})

	if streamer.outputIndex != before+2 {
		t.Fatalf("expected output_index to advance by 2 after two web_search_call items (before=%d, after=%d)", before, streamer.outputIndex)
	}
}
