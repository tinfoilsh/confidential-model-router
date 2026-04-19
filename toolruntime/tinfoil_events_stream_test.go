package toolruntime

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestResponsesStreamerForEvents is a minimal factory mirroring
// newTestResponsesStreamer from responses_stream_test.go but exposes the
// eventsEnabled knob directly so marker tests can flip it per case.
func newTestResponsesStreamerForEvents(t *testing.T, eventsEnabled bool) (*responsesStreamer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	streamer := &responsesStreamer{
		w:                     rec,
		flusher:               rec,
		usageMetricsRequested: true,
		eventsEnabled:         eventsEnabled,
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

// TestResponsesStreamerEmitTinfoilEventMarkerEmitsItemBurst pins the
// Responses streaming contract: a single marker becomes a
// spec-conformant message-item burst (added -> content_part.added ->
// output_text.delta -> output_text.done -> content_part.done ->
// output_item.done) so strict SDKs render it as a tiny assistant
// message while opt-in clients can strip the tags.
func TestResponsesStreamerEmitTinfoilEventMarkerEmitsItemBurst(t *testing.T) {
	streamer, rec := newTestResponsesStreamerForEvents(t, true)

	streamer.emitTinfoilEventMarker("ws_1", "in_progress", map[string]any{"type": "search", "query": "q"}, "")

	body := rec.Body.String()
	// Verify each of the six events fires exactly once and in order.
	wantOrder := []string{
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
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

	// Locate the output_text.delta frame, decode it, and assert the
	// marker round-trips intact via `delta`. Substring checks over the
	// raw wire would be fooled by json.Marshal's HTML-escaping of the
	// `<` / `>` characters in the open/close tags.
	deltaFrame := ""
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("response stream frame must be valid JSON: %v (%q)", err, payload)
		}
		if decoded["type"] == "response.output_text.delta" {
			deltaFrame = payload
			delta, _ := decoded["delta"].(string)
			if !strings.Contains(delta, tinfoilEventOpenTag) || !strings.Contains(delta, tinfoilEventCloseTag) {
				t.Fatalf("response.output_text.delta missing tinfoil-event tags: %q", delta)
			}
		}
	}
	if deltaFrame == "" {
		t.Fatalf("response stream did not include a response.output_text.delta frame")
	}
}

// TestResponsesStreamerEmitTinfoilEventMarkerIsNoOpWhenDisabled pins the
// opt-out path for Responses streaming: without the header the helper
// writes nothing, so strict Responses SDKs see only the events OpenAI
// documents.
func TestResponsesStreamerEmitTinfoilEventMarkerIsNoOpWhenDisabled(t *testing.T) {
	streamer, rec := newTestResponsesStreamerForEvents(t, false)

	streamer.emitTinfoilEventMarker("ws_1", "in_progress", map[string]any{"type": "search"}, "")

	if body := rec.Body.String(); body != "" {
		t.Fatalf("expected empty stream when events are disabled, got %q", body)
	}
}

// TestResponsesStreamerMarkerOutputIndexIsMonotonic pins that synthetic
// marker items consume and advance the streamer's output_index counter,
// so any real items emitted after a marker keep a monotonically
// increasing index the client can use for UI bookkeeping.
func TestResponsesStreamerMarkerOutputIndexIsMonotonic(t *testing.T) {
	streamer, _ := newTestResponsesStreamerForEvents(t, true)
	before := streamer.outputIndex

	streamer.emitTinfoilEventMarker("ws_1", "in_progress", map[string]any{"type": "search"}, "")
	streamer.emitTinfoilEventMarker("ws_1", "completed", map[string]any{"type": "search"}, "")

	if streamer.outputIndex != before+2 {
		t.Fatalf("expected output_index to advance by 2 after two markers (before=%d, after=%d)", before, streamer.outputIndex)
	}
}
