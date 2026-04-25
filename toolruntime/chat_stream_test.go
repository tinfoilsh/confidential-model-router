package toolruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newTestChatStreamer builds a chatStreamer backed by an httptest recorder,
// pre-flipped into "headers written" state so tests can exercise chunk
// emission without driving a real upstream request.
func newTestChatStreamer(t *testing.T) (*chatStreamer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	streamer := &chatStreamer{
		streamBase: streamBase{
			w:                     rec,
			flusher:               rec,
			usageMetricsRequested: true,
			citations:             &citationState{nextIndex: 1},
			toolCalls:             &toolCallLog{},
			usageTotals:           &usageAccumulator{},
			model:                 "gpt-oss-120b",
			headersWritten:        true,
		},
		clientRequestedUsage: true,
		id:                   "chatcmpl_test",
		created:              1700000000,
	}
	streamer.emitter = newCitationEmitter(streamer.citations)
	return streamer, rec
}

func TestChatStreamerPumpEmitsContentAndToolCalls(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	upstream := strings.Join([]string{
		`data: {"id":"up_1","created":1700000001,"model":"gpt-oss-120b","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"content":"hello "}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"content":"world"}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"query\":\"go\"}"}}]}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	reader := newSSEReader(strings.NewReader(upstream))

	result, err := streamer.pumpUpstream(reader)
	if err != nil {
		t.Fatalf("pumpUpstream returned error: %v", err)
	}
	if result.content != "hello world" {
		t.Fatalf("unexpected accumulated content: %q", result.content)
	}
	if result.finishReason != "tool_calls" {
		t.Fatalf("unexpected finish_reason: %q", result.finishReason)
	}
	if len(result.toolCalls) != 1 || result.toolCalls[0].name != "search" {
		t.Fatalf("unexpected tool_calls: %#v", result.toolCalls)
	}
	if got := result.toolCalls[0].arguments["query"]; got != "go" {
		t.Fatalf("unexpected tool argument: %#v", got)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hello "`) || !strings.Contains(body, `"content":"world"`) {
		t.Fatalf("expected streamed content chunks, got %s", body)
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("tool_calls must not be forwarded during pumpUpstream, got %s", body)
	}

	usage := streamer.finalUsage()
	if usage == nil {
		t.Fatal("expected aggregated usage after streaming turn")
	}
	if usage["prompt_tokens"].(int) != 3 || usage["completion_tokens"].(int) != 2 {
		t.Fatalf("unexpected aggregated usage: %#v", usage)
	}
}

func TestChatStreamerFinalizeEmitsFinishAndUsage(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	streamer.usageTotals.Add(&upstreamJSONResponse{body: map[string]any{
		"usage": map[string]any{
			"prompt_tokens":     float64(4),
			"completion_tokens": float64(6),
			"total_tokens":      float64(10),
		},
	}})
	result := chatIterationResult{
		finishReason: "stop",
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// nil enclave manager is fine because the auth header is absent so
	// emitBillingEvent short-circuits before dereferencing it.
	if err := streamer.finalize(req, nil, "gpt-oss-120b", result, nil); err != nil {
		t.Fatalf("finalize returned error: %v", err)
	}

	body := rec.Body.String()
	for _, fragment := range []string{
		`"finish_reason":"stop"`,
		`"usage":{"completion_tokens":6`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("expected streamed body to contain %q, got %s", fragment, body)
		}
	}
}

func TestChatStreamerFinalizeForcesToolCallsFinishReason(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	result := chatIterationResult{
		finishReason: "stop",
		rawToolCalls: []any{
			map[string]any{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name":      "client_lookup",
					"arguments": `{"id":1}`,
				},
			},
		},
	}
	clientToolCalls := []toolCall{
		{id: "call_1", name: "client_lookup", arguments: map[string]any{"id": float64(1)}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if err := streamer.finalize(req, nil, "gpt-oss-120b", result, clientToolCalls); err != nil {
		t.Fatalf("finalize returned error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected finish_reason tool_calls, got %s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("unexpected stop finish_reason when tool_calls are present, got %s", body)
	}
}

func TestChatStreamerEmitContentDeltaForwardsAnnotations(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	streamer.citations.sources = []citationSource{
		{index: 1, url: "https://example.com/a", title: "A"},
	}
	streamer.emitContentDelta("See ")
	streamer.emitContentDelta("[A](https://example.com/a)")
	streamer.emitContentDelta(" for details.")
	streamer.flushCitations()

	body := rec.Body.String()
	if !strings.Contains(body, `"annotations":[{"type":"url_citation"`) {
		t.Fatalf("expected annotation chunk in body, got %s", body)
	}
	if !strings.Contains(body, "example.com") {
		t.Fatalf("expected citation url in body, got %s", body)
	}
}

func TestChatStreamerPumpSurfacesMidStreamError(t *testing.T) {
	streamer, _ := newTestChatStreamer(t)
	upstream := strings.Join([]string{
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"error":{"message":"ctx exceeded","type":"invalid_request_error"}}`,
		"",
	}, "\n\n")
	reader := newSSEReader(strings.NewReader(upstream))

	_, err := streamer.pumpUpstream(reader)
	if err == nil {
		t.Fatal("expected mid-stream error to surface as upstreamError, got nil")
	}
	upErr, ok := err.(*upstreamError)
	if !ok {
		t.Fatalf("expected *upstreamError, got %T", err)
	}
	if !strings.Contains(string(upErr.body), "ctx exceeded") {
		t.Fatalf("expected error body to carry message, got %s", upErr.body)
	}
}

func TestChatStreamerTerminateWithErrorEmitsErrorFrame(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	err := &upstreamError{
		statusCode: http.StatusBadGateway,
		header:     http.Header{"Content-Type": []string{"application/json"}},
		body:       []byte(`{"error":{"message":"boom"}}`),
	}
	if termErr := streamer.terminateWithError(req, nil, "gpt-oss-120b", err); termErr != nil {
		t.Fatalf("terminateWithError returned error: %v", termErr)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error":{`) {
		t.Fatalf("expected error frame, got %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected stream terminator after error, got %s", body)
	}
}

func TestChatStreamerPinsIdentityAcrossIterations(t *testing.T) {
	streamer, _ := newTestChatStreamer(t)
	// Clear pre-seeded identity so ensureStreamIdentity captures from
	// upstream the same way it does in production.
	streamer.id = ""
	streamer.created = 0
	streamer.model = ""

	first := strings.Join([]string{
		`data: {"id":"up_first","created":1700000100,"model":"gpt-oss-120b-abc","choices":[{"index":0,"delta":{"content":"a"}}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	second := strings.Join([]string{
		`data: {"id":"up_second","created":1700000200,"model":"gpt-oss-120b-xyz","choices":[{"index":0,"delta":{"content":"b"}}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")

	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(first))); err != nil {
		t.Fatalf("first pump err: %v", err)
	}
	firstID, firstCreated, firstModel := streamer.id, streamer.created, streamer.model
	if firstID != "up_first" || firstCreated != 1700000100 || firstModel != "gpt-oss-120b-abc" {
		t.Fatalf("first iteration identity = (%q,%d,%q), want (up_first,1700000100,gpt-oss-120b-abc)", firstID, firstCreated, firstModel)
	}
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(second))); err != nil {
		t.Fatalf("second pump err: %v", err)
	}
	if streamer.id != firstID || streamer.created != firstCreated || streamer.model != firstModel {
		t.Fatalf("identity drifted after second iteration: (%q,%d,%q)", streamer.id, streamer.created, streamer.model)
	}
}

// TestChatStreamerEmitsUpstreamIdentityOnRoleChunk pins a key production
// contract that a helper-bypass test would silently miss: every chunk
// emitted to the client, INCLUDING the initial assistant role-delta
// chunk, carries upstream's id / created / model -- never a
// router-synthesized placeholder. The role chunk is deferred until the
// first upstream frame is observed so identity is always available.
func TestChatStreamerEmitsUpstreamIdentityOnRoleChunk(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	// Reset identity and roleEmitted so the pump has to capture from
	// upstream exactly the way it does in production after
	// writeSSEHeaders has run (but before any frame has been read).
	streamer.id = ""
	streamer.created = 0
	streamer.model = ""
	streamer.roleEmitted = false

	upstream := strings.Join([]string{
		`data: {"id":"up_real","created":1700000100,"model":"gpt-oss-120b-0416","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream))); err != nil {
		t.Fatalf("pump err: %v", err)
	}
	if streamer.id != "up_real" {
		t.Fatalf("expected upstream id adopted, got %q", streamer.id)
	}
	if streamer.created != 1700000100 {
		t.Fatalf("expected upstream created adopted, got %d", streamer.created)
	}
	if streamer.model != "gpt-oss-120b-0416" {
		t.Fatalf("expected upstream-resolved model adopted, got %q", streamer.model)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"up_real"`) {
		t.Fatalf("expected every chunk to carry upstream id, got %s", body)
	}
	if !strings.Contains(body, `"created":1700000100`) {
		t.Fatalf("expected every chunk to carry upstream created, got %s", body)
	}
	if !strings.Contains(body, `"model":"gpt-oss-120b-0416"`) {
		t.Fatalf("expected every chunk to carry upstream model, got %s", body)
	}
	// The initial role chunk must precede the first content delta and
	// must also carry upstream identity.
	rolePos := strings.Index(body, `"role":"assistant"`)
	contentPos := strings.Index(body, `"content":"x"`)
	if rolePos < 0 || contentPos < 0 || rolePos > contentPos {
		t.Fatalf("expected role chunk before content chunk, got role=%d content=%d body=%s", rolePos, contentPos, body)
	}
	// No router-minted placeholders should appear anywhere in the body.
	if strings.Contains(body, "chatcmpl-") {
		t.Fatalf("expected no router-minted id placeholder, got %s", body)
	}
}

// TestChatStreamerPumpSurfacesAbruptDisconnect pins the contract that an
// upstream stream which terminates without a `[DONE]` marker is reported
// to the caller as an upstream error, so the client sees a terminating
// error frame rather than a silently truncated chat completion.
func TestChatStreamerPumpSurfacesAbruptDisconnect(t *testing.T) {
	streamer, _ := newTestChatStreamer(t)
	upstream := strings.Join([]string{
		`data: {"id":"up","choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		"",
	}, "\n\n")
	_, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)))
	if err == nil {
		t.Fatal("expected abrupt EOF to surface as error")
	}
	upErr, ok := err.(*upstreamError)
	if !ok {
		t.Fatalf("expected *upstreamError, got %T", err)
	}
	if !strings.Contains(string(upErr.body), "terminal [DONE] marker") {
		t.Fatalf("expected disconnect-specific error body, got %s", upErr.body)
	}
}

func TestChatToolCallBuilderAssemblesFragments(t *testing.T) {
	builder := &chatToolCallBuilder{}
	builder.ingest([]any{
		map[string]any{"index": float64(0), "id": "call_1", "type": "function", "function": map[string]any{"name": "search"}},
	})
	builder.ingest([]any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `{"query":"`}},
	})
	builder.ingest([]any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `go"}`}},
	})

	calls := builder.toolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].name != "search" || calls[0].arguments["query"] != "go" {
		t.Fatalf("unexpected reassembled call: %#v", calls[0])
	}
	raw := builder.raw()
	if len(raw) != 1 {
		t.Fatalf("expected 1 raw call, got %d", len(raw))
	}
	rawMap, _ := raw[0].(map[string]any)
	fn, _ := rawMap["function"].(map[string]any)
	if fn["arguments"].(string) != `{"query":"go"}` {
		t.Fatalf("unexpected raw arguments: %#v", fn["arguments"])
	}
}

func TestRawToolCallsFromParsedMatchesClientCallsWithoutIDs(t *testing.T) {
	raw := []any{
		map[string]any{
			"id":   "",
			"type": "function",
			"function": map[string]any{
				"name":      "search",
				"arguments": `{"query":"go"}`,
			},
		},
		map[string]any{
			"id":   "",
			"type": "function",
			"function": map[string]any{
				"name":      "client_lookup",
				"arguments": `{"id":1}`,
			},
		},
	}
	parsed := []toolCall{
		{name: "client_lookup", arguments: map[string]any{"id": float64(1)}},
	}

	filtered := rawToolCallsFromParsed(parsed, raw)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered raw tool call, got %#v", filtered)
	}
	call, _ := filtered[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if got := function["name"]; got != "client_lookup" {
		t.Fatalf("expected client_lookup raw tool call, got %#v", got)
	}
	if got := call["index"]; got != 0 {
		t.Fatalf("expected reindexed client tool call at 0, got %#v", got)
	}
}

// TestChatStreamerMalformedJSONFailsStream pins that a malformed
// upstream SSE frame terminates the stream with an upstreamError
// instead of silently dropping the chunk.
func TestChatStreamerMalformedJSONFailsStream(t *testing.T) {
	streamer, _ := newTestChatStreamer(t)
	upstream := strings.Join([]string{
		`data: {"id":"cmpl","choices":[{"index":0`,
		"",
	}, "\n\n")
	_, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)))
	if err == nil {
		t.Fatal("expected malformed JSON to surface as error")
	}
	upErr, ok := err.(*upstreamError)
	if !ok {
		t.Fatalf("expected *upstreamError, got %T", err)
	}
	if !strings.Contains(string(upErr.body), "malformed SSE JSON") {
		t.Fatalf("expected malformed-JSON body, got %s", string(upErr.body))
	}
}

// failingFlushWriter is an http.ResponseWriter+Flusher whose Write fails
// with io.ErrClosedPipe to simulate a client disconnect mid-stream.
type failingFlushWriter struct {
	header http.Header
}

func (f *failingFlushWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *failingFlushWriter) Write(_ []byte) (int, error) { return 0, io.ErrClosedPipe }
func (f *failingFlushWriter) WriteHeader(_ int)           {}
func (f *failingFlushWriter) Flush()                      {}

// TestChatStreamerPumpAbortsOnClientDisconnect pins that once a write to
// the client returns an error (client hung up), pumpUpstream stops
// reading upstream frames and returns so the outer loop does not spend
// more tokens or MCP tool calls on a caller that has disconnected.
func TestChatStreamerPumpAbortsOnClientDisconnect(t *testing.T) {
	w := &failingFlushWriter{}
	streamer := &chatStreamer{
		streamBase: streamBase{
			w:              w,
			flusher:        w,
			citations:      &citationState{nextIndex: 1},
			toolCalls:      &toolCallLog{},
			usageTotals:    &usageAccumulator{},
			headersWritten: true,
			model:          "gpt-oss-120b",
		},
		id:      "cmpl",
		created: 1,
	}
	streamer.emitter = newCitationEmitter(streamer.citations)

	// A long upstream stream; if writeErr is not respected the pump would
	// keep iterating. We assert it stops fast and returns the write error.
	frames := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		frames = append(frames, `data: {"id":"up","choices":[{"index":0,"delta":{"content":"x"}}]}`)
	}
	frames = append(frames, "data: [DONE]", "")
	upstream := strings.Join(frames, "\n\n")
	_, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream)))
	if err == nil {
		t.Fatalf("expected pumpUpstream to surface the write error, got nil")
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe, got %v", err)
	}
	if streamer.writeErr == nil {
		t.Fatalf("expected streamer.writeErr to be latched")
	}
}

// TestChatStreamerPumpResolvesMarkdownAnnotations drives a real SSE
// pump with upstream content that contains a `[label](url)` markdown
// span, asserts that the pump forwards a delta.annotations chunk, and
// that the annotation's url matches a pre-registered source. This
// guards the end-to-end wiring between pumpUpstream → emitContentDelta
// → citationEmitter → delta.annotations that a caller-facing regression
// would break silently even when the unit-level emitter test still
// passes (for example by dropping the annotations branch of
// emitContentDelta, or by feeding content through a path that skips
// the emitter).
func TestChatStreamerPumpResolvesMarkdownAnnotations(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	streamer.citations.sources = []citationSource{
		{index: 1, url: "https://example.com/a", title: "A"},
	}
	upstream := strings.Join([]string{
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"content":"See "}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"content":"[A](https://example.com/a)"}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"content":" for details."}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(upstream))); err != nil {
		t.Fatalf("pump err: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"annotations":[{"type":"url_citation"`) {
		t.Fatalf("expected delta.annotations chunk in streamed body, got %s", body)
	}
	if !strings.Contains(body, `"url":"https://example.com/a"`) {
		t.Fatalf("expected annotation url in streamed body, got %s", body)
	}
}

// TestChatStreamerPumpForwardsReasoningDelta pins that reasoning_content
// and reasoning delta fields produced by upstream reasoning models pass
// through the router to the client unchanged so the live "thinking" UI
// keeps updating while the web search tool loop is active.
func TestChatStreamerPumpForwardsReasoningDelta(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	upstream := strings.Join([]string{
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"reasoning_content":"let me think..."}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"reasoning":"consider both"}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		`data: {"id":"up_1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	reader := newSSEReader(strings.NewReader(upstream))

	if _, err := streamer.pumpUpstream(reader); err != nil {
		t.Fatalf("pumpUpstream returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"reasoning_content":"let me think..."`) {
		t.Fatalf("expected reasoning_content forwarded to client, got %s", body)
	}
	if !strings.Contains(body, `"reasoning":"consider both"`) {
		t.Fatalf("expected reasoning forwarded to client, got %s", body)
	}
}

// chatToolCallTurn builds an SSE chunk sequence for one iteration where
// the upstream model emits a tool call and finishes with
// finish_reason:"tool_calls". Used by multi-iteration tests to simulate
// the router's tool loop pumping multiple upstream turns through a
// single client stream.
func chatToolCallTurn(upstreamID, callID, toolName, args string) string {
	return strings.Join([]string{
		`data: {"id":"` + upstreamID + `","created":1700000001,"model":"gpt-oss-120b","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"` + upstreamID + `","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + callID + `","type":"function","function":{"name":"` + toolName + `","arguments":` + jsonStringChat(args) + `}}]}}]}`,
		`data: {"id":"` + upstreamID + `","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"data: [DONE]",
		"",
	}, "\n\n")
}

// chatFinalTextTurn builds an SSE chunk sequence for the terminal
// iteration where the upstream model emits assistant text and finishes
// with finish_reason:"stop".
func chatFinalTextTurn(upstreamID, text string) string {
	return strings.Join([]string{
		`data: {"id":"` + upstreamID + `","created":1700000001,"model":"gpt-oss-120b","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"` + upstreamID + `","choices":[{"index":0,"delta":{"content":` + jsonStringChat(text) + `}}]}`,
		`data: {"id":"` + upstreamID + `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"data: [DONE]",
		"",
	}, "\n\n")
}

func jsonStringChat(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestChatStreamerTwoIterationStitching pins the N=2 chat multi-turn
// invariants: the client sees exactly one role-delta chunk, identity
// (id / created / model) stays pinned to the first upstream turn's
// values across both iterations, and tool_calls frames are buffered
// into the iteration result rather than forwarded live (the outer loop
// classifies them as router-owned or client-owned after the pump).
func TestChatStreamerTwoIterationStitching(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	streamer.id = ""
	streamer.created = 0
	streamer.model = ""
	streamer.roleEmitted = false

	// Iteration 0: upstream emits a tool call. The pump buffers it
	// into result.toolCalls and does NOT forward it to the client.
	result0, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(
		chatToolCallTurn("up_iter0", "call_a", "search", `{"query":"go"}`),
	)))
	if err != nil {
		t.Fatalf("iter0 pump err: %v", err)
	}
	if len(result0.toolCalls) != 1 || result0.toolCalls[0].name != "search" {
		t.Fatalf("iter0 expected 1 tool call buffered, got %+v", result0.toolCalls)
	}

	// Iteration 1: upstream emits the final answer. Identity must stay
	// pinned to iter0's id / created / model.
	if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(
		chatFinalTextTurn("up_iter1", "done"),
	))); err != nil {
		t.Fatalf("iter1 pump err: %v", err)
	}

	body := rec.Body.String()
	if n := strings.Count(body, `"delta":{"role":"assistant"}`); n != 1 {
		t.Fatalf("expected exactly 1 role-delta chunk across 2 iterations, got %d", n)
	}
	if strings.Contains(body, `"id":"up_iter1"`) {
		t.Fatalf("iter1 upstream id leaked to client: %s", body)
	}
	if !strings.Contains(body, `"id":"up_iter0"`) {
		t.Fatalf("expected up_iter0 id pinned across iterations, got %s", body)
	}
	// Pump buffers all tool_calls; none should appear on the client
	// stream within these two pump invocations.
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("pump must buffer tool_calls, got %s", body)
	}
	if !strings.Contains(body, `"content":"done"`) {
		t.Fatalf("expected final content 'done' forwarded to client, got %s", body)
	}
}

// TestChatStreamerThreeIterationStitching pins the N=3 chat path: two
// consecutive tool-call turns before a final message. Identity stays
// pinned to iter0 across all three turns, client sees exactly one
// role chunk, and no tool_calls frames leak out of the pump.
func TestChatStreamerThreeIterationStitching(t *testing.T) {
	streamer, rec := newTestChatStreamer(t)
	streamer.id = ""
	streamer.created = 0
	streamer.model = ""
	streamer.roleEmitted = false

	turns := []string{
		chatToolCallTurn("up_iter0", "call_a", "search", `{"query":"q1"}`),
		chatToolCallTurn("up_iter1", "call_b", "search", `{"query":"q2"}`),
		chatFinalTextTurn("up_iter2", "answer"),
	}
	for i, turn := range turns {
		if _, err := streamer.pumpUpstream(newSSEReader(strings.NewReader(turn))); err != nil {
			t.Fatalf("iter%d pump err: %v", i, err)
		}
	}

	body := rec.Body.String()
	if n := strings.Count(body, `"delta":{"role":"assistant"}`); n != 1 {
		t.Fatalf("expected 1 role-delta chunk across 3 iterations, got %d", n)
	}
	if !strings.Contains(body, `"id":"up_iter0"`) {
		t.Fatalf("iter0 id must be pinned across all iterations")
	}
	for _, leaked := range []string{`"id":"up_iter1"`, `"id":"up_iter2"`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("later upstream id leaked to client: %s in %s", leaked, body)
		}
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("pump must buffer all tool_calls across 3 iterations")
	}
	if !strings.Contains(body, `"content":"answer"`) {
		t.Fatalf("expected final content 'answer' forwarded to client, got %s", body)
	}
}

// TestChatStreamerFallbacksLogOnceWhenUpstreamOmitsIdentity pins that
// the id and created fallbacks, which exist as defense in depth against
// a misbehaving upstream, emit a single log line each when they actually
// fire. Silent fallbacks hide fleet regressions; logging gives operators
// a breadcrumb when an upstream starts dropping OpenAI-required fields.
func TestChatStreamerFallbacksLogOnceWhenUpstreamOmitsIdentity(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	streamer, _ := newTestChatStreamer(t)
	streamer.id = ""
	streamer.created = 0

	if got := streamer.streamID(); !strings.HasPrefix(got, "chatcmpl-") {
		t.Fatalf("streamID fallback should be router-minted, got %q", got)
	}
	if got := streamer.streamCreated(); got <= 0 {
		t.Fatalf("streamCreated fallback should be a positive unix time, got %d", got)
	}
	// Subsequent calls must NOT relog; the fallback is idempotent.
	_ = streamer.streamID()
	_ = streamer.streamCreated()

	logged := buf.String()
	if idHits := strings.Count(logged, "upstream omitted chat.completion.chunk id"); idHits != 1 {
		t.Fatalf("expected exactly one id-fallback log line, got %d in %q", idHits, logged)
	}
	if createdHits := strings.Count(logged, "upstream omitted chat.completion.chunk created"); createdHits != 1 {
		t.Fatalf("expected exactly one created-fallback log line, got %d in %q", createdHits, logged)
	}
}

// TestChatStreamerDoesNotLogWhenUpstreamProvidesIdentity pins the
// inverse: the happy path (vLLM always emits id/created) must produce
// zero log noise. A chatty router on every request would bury real
// signals.
func TestChatStreamerDoesNotLogWhenUpstreamProvidesIdentity(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	streamer, _ := newTestChatStreamer(t)
	// newTestChatStreamer pre-seeds id and created, mirroring a
	// well-behaved upstream that populated them on the first chunk.
	_ = streamer.streamID()
	_ = streamer.streamCreated()
	_ = streamer.streamID()

	if logged := buf.String(); logged != "" {
		t.Fatalf("happy path must not log; got %q", logged)
	}
}
