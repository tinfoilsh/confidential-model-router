package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

// sinkResponseWriter accepts any write sequence. httptest.ResponseRecorder
// can't be used for 1xx tests: it latches the informational code as the
// final status and then rejects the body, which a real server would accept.
type sinkResponseWriter struct{ header http.Header }

func newSinkResponseWriter() *sinkResponseWriter {
	return &sinkResponseWriter{header: make(http.Header)}
}

func (s *sinkResponseWriter) Header() http.Header         { return s.header }
func (s *sinkResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (s *sinkResponseWriter) WriteHeader(int)             {}

func histogramState(t *testing.T, o prometheus.Observer) (count uint64, sum float64) {
	t.Helper()
	m, ok := o.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer is not a metric: %T", o)
	}
	pb := &dto.Metric{}
	if err := m.Write(pb); err != nil {
		t.Fatalf("failed to read histogram: %v", err)
	}
	return pb.GetHistogram().GetSampleCount(), pb.GetHistogram().GetSampleSum()
}

func TestLatencyWriterObservesStreaming(t *testing.T) {
	const model = "latency-test-streaming"
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	lw := &latencyWriter{
		ResponseWriter: httptest.NewRecorder(),
		model:          model,
		enclave:        "enclave-1",
		pool:           "reserved",
		class:          "configured",
		arrival:        now,
		start:          now,
		observeLegacy:  true,
		nowFunc:        func() time.Time { return now },
	}

	// First byte 500ms after dispatch, then three chunks at 20ms gaps
	now = now.Add(500 * time.Millisecond)
	lw.WriteHeader(200)
	if _, err := lw.Write([]byte("chunk")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(20 * time.Millisecond)
		if _, err := lw.Write([]byte("chunk")); err != nil {
			t.Fatal(err)
		}
	}

	ttftCount, ttftSum := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "configured"))
	if ttftCount != 1 || ttftSum != 0.5 {
		t.Fatalf("expected one TTFT observation of 0.5s, got count=%d sum=%v", ttftCount, ttftSum)
	}
	replicaTTFTCount, replicaTTFTSum := histogramState(t, manager.ReplicaTTFTSeconds.WithLabelValues(model, "enclave-1", "reserved", "configured"))
	if replicaTTFTCount != 1 || replicaTTFTSum != 0.5 {
		t.Fatalf("expected one per-replica TTFT observation of 0.5s, got count=%d sum=%v", replicaTTFTCount, replicaTTFTSum)
	}
	itlCount, itlSum := histogramState(t, manager.InterTokenSeconds.WithLabelValues(model, "configured"))
	if itlCount != 3 || itlSum < 0.0599 || itlSum > 0.0601 {
		t.Fatalf("expected three 20ms inter-token observations, got count=%d sum=%v", itlCount, itlSum)
	}
}

func TestLatencyWriterSkipsErrorResponses(t *testing.T) {
	const model = "latency-test-error"
	lw := &latencyWriter{
		ResponseWriter: httptest.NewRecorder(),
		model:          model,
		enclave:        "enclave-1",
		pool:           "shared",
		class:          "none",
		start:          time.Now(),
		observeLegacy:  true,
	}

	lw.WriteHeader(502)
	if _, err := lw.Write([]byte(`{"error":{}}`)); err != nil {
		t.Fatal(err)
	}

	if count, _ := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "none")); count != 0 {
		t.Fatalf("error response must not observe TTFT, got count=%d", count)
	}
	if count, _ := histogramState(t, manager.ReplicaTTFTSeconds.WithLabelValues(model, "enclave-1", "shared", "none")); count != 0 {
		t.Fatalf("error response must not observe per-replica TTFT, got count=%d", count)
	}
}

func TestLatencyWriterInformationalThenError(t *testing.T) {
	const model = "latency-test-1xx-error"
	lw := &latencyWriter{
		ResponseWriter: newSinkResponseWriter(),
		model:          model,
		class:          "none",
		start:          time.Now(),
		observeLegacy:  true,
	}

	lw.WriteHeader(100)
	lw.WriteHeader(502)
	if _, err := lw.Write([]byte(`{"error":{}}`)); err != nil {
		t.Fatal(err)
	}
	if count, _ := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "none")); count != 0 {
		t.Fatalf("error response behind a 1xx must not observe TTFT, got count=%d", count)
	}
}

func TestLatencyWriterInformationalThenSuccess(t *testing.T) {
	const model = "latency-test-1xx-ok"
	lw := &latencyWriter{
		ResponseWriter: newSinkResponseWriter(),
		model:          model,
		class:          "none",
		arrival:        time.Now(),
		start:          time.Now(),
		observeLegacy:  true,
	}

	lw.WriteHeader(100)
	lw.WriteHeader(200)
	if _, err := lw.Write([]byte("chunk")); err != nil {
		t.Fatal(err)
	}
	if count, _ := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "none")); count != 1 {
		t.Fatalf("success behind a 1xx must observe TTFT, got count=%d", count)
	}
}

func TestLatencyWriterImplicit200(t *testing.T) {
	const model = "latency-test-implicit"
	lw := &latencyWriter{
		ResponseWriter: httptest.NewRecorder(),
		model:          model,
		class:          "none",
		arrival:        time.Now(),
		start:          time.Now(),
		observeLegacy:  true,
	}

	// Write without WriteHeader is an implicit 200 and must be observed
	if _, err := lw.Write([]byte("chunk")); err != nil {
		t.Fatal(err)
	}
	if count, _ := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "none")); count != 1 {
		t.Fatalf("implicit 200 must observe TTFT, got count=%d", count)
	}
}

func TestLatencyWriterForwardsFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := newLatencyWriter(rec, time.Now(), "latency-test-flush", "enclave-1", "none", "none")
	lw.Flush()
	if !rec.Flushed {
		t.Fatal("Flush was not forwarded to the underlying writer")
	}
}

func counterState(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	pb := &dto.Metric{}
	if err := c.Write(pb); err != nil {
		t.Fatalf("failed to read counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

// newSSELatencyWriter builds a writer over a recorder that already carries
// the SSE content type, with time controlled through *now. Labels are fixed
// so tests only vary the model name.
func newSSELatencyWriter(model string, now *time.Time) *latencyWriter {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	return &latencyWriter{
		ResponseWriter: rec,
		model:          model,
		enclave:        "enclave-1",
		pool:           "reserved",
		class:          "configured",
		arrival:        *now,
		start:          *now,
		observeLegacy:  true,
		nowFunc:        func() time.Time { return *now },
	}
}

func writeChunk(t *testing.T, lw *latencyWriter, s string) {
	t.Helper()
	if _, err := lw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

func firstTokenState(t *testing.T, model string) (uint64, float64) {
	t.Helper()
	return histogramState(t, manager.FirstTokenSeconds.WithLabelValues(model, "enclave-1", "reserved", "configured"))
}

func noFirstTokenCount(t *testing.T, model, reason string) float64 {
	t.Helper()
	return counterState(t, manager.NoFirstTokenTotal.WithLabelValues(model, "enclave-1", "reserved", "configured", reason))
}

func TestFirstTokenIgnoresChatControlEvents(t *testing.T) {
	const model = "ft-chat-control"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	lw := newSSELatencyWriter(model, &now)

	lw.WriteHeader(200)
	now = t0.Add(200 * time.Millisecond)
	writeChunk(t, lw, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
	now = t0.Add(400 * time.Millisecond)
	writeChunk(t, lw, ": ping\n\n")
	now = t0.Add(900 * time.Millisecond)
	writeChunk(t, lw, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n")

	count, sum := firstTokenState(t, model)
	if count != 1 || sum < 0.899 || sum > 0.901 {
		t.Fatalf("expected one first-token observation of 0.9s, got count=%d sum=%v", count, sum)
	}

	// A stream that produced a token must not be counted as token-less.
	lw.finish(context.Background())
	if got := noFirstTokenCount(t, model, "no_output"); got != 0 {
		t.Fatalf("stream with a token counted as no_output: %v", got)
	}
}

func TestFirstTokenIgnoresResponsesControlEvents(t *testing.T) {
	const model = "ft-responses-control"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	lw := newSSELatencyWriter(model, &now)

	lw.WriteHeader(200)
	now = t0.Add(100 * time.Millisecond)
	writeChunk(t, lw, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"instructions\":\"be helpful\"}}\n\n")
	now = t0.Add(150 * time.Millisecond)
	writeChunk(t, lw, "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	now = t0.Add(2 * time.Second)
	writeChunk(t, lw, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"content\":[]}}\n\n")
	writeChunk(t, lw, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
	now = t0.Add(2500 * time.Millisecond)
	writeChunk(t, lw, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"He\"}\n\n")

	count, sum := firstTokenState(t, model)
	if count != 1 || sum < 2.499 || sum > 2.501 {
		t.Fatalf("expected one first-token observation of 2.5s, got count=%d sum=%v", count, sum)
	}
}

func TestFirstTokenGeneratedShapes(t *testing.T) {
	// Every event shape that carries generated output must stop the clock.
	for name, chunk := range map[string]string{
		"completions text":   "data: {\"choices\":[{\"index\":0,\"text\":\"Hi\"}]}\n\n",
		"reasoning content":  "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Th\"}}]}\n\n",
		"tool calls":         "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n",
		"refusal":            "data: {\"choices\":[{\"delta\":{\"refusal\":\"I\"}}]}\n\n",
		"responses fn args":  "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"lo\"}\n\n",
		"second choice only": "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"}},{\"index\":1,\"delta\":{\"content\":\"x\"}}]}\n\n",
	} {
		model := "ft-shape-" + strings.ReplaceAll(name, " ", "-")
		t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		now := t0
		lw := newSSELatencyWriter(model, &now)
		lw.WriteHeader(200)
		now = t0.Add(time.Second)
		writeChunk(t, lw, chunk)
		if count, _ := firstTokenState(t, model); count != 1 {
			t.Errorf("%s: expected a first-token observation, got count=%d", name, count)
		}
	}
}

func TestFirstTokenSplitAcrossWrites(t *testing.T) {
	const model = "ft-split"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	lw := newSSELatencyWriter(model, &now)

	lw.WriteHeader(200)
	const event = "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	now = t0.Add(time.Second)
	writeChunk(t, lw, event[:20])
	now = t0.Add(3 * time.Second)
	writeChunk(t, lw, event[20:])

	count, sum := firstTokenState(t, model)
	if count != 1 || sum < 2.999 || sum > 3.001 {
		t.Fatalf("split event must observe at completion time, got count=%d sum=%v", count, sum)
	}
}

func TestFirstTokenSSESpecEdgeCases(t *testing.T) {
	// CRLF endings, no space after the colon, and multi-line data joined
	// per the SSE spec must all classify correctly.
	d := &sseTokenDetector{}
	if d.feed([]byte("data:{\"choices\":[{\"delta\":{\"content\":\"\"}}]}\r\n\r\n")) {
		t.Fatal("empty CRLF delta classified as a token")
	}
	if !d.feed([]byte("data:{\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\r\n\r\n")) {
		t.Fatal("CRLF token event missed")
	}
	d = &sseTokenDetector{}
	if !d.feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]\ndata: }\n\n")) {
		t.Fatal("multi-line data event missed")
	}
}

func TestFirstTokenSkipsOversizedControlFrame(t *testing.T) {
	const model = "ft-oversize"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	lw := newSSELatencyWriter(model, &now)
	lw.WriteHeader(200)

	// A response.created echoing a huge prompt, exceeding the scan buffer,
	// delivered across several writes. It must neither classify nor wedge
	// the detector.
	huge := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"instructions\":\"" +
		strings.Repeat("a", maxSSEBufferBytes+1024) + "\"}}\n\n"
	now = t0.Add(time.Second)
	for len(huge) > 0 {
		n := min(len(huge), 32<<10)
		writeChunk(t, lw, huge[:n])
		huge = huge[n:]
	}
	if count, _ := firstTokenState(t, model); count != 0 {
		t.Fatal("oversized control frame classified as a token")
	}

	now = t0.Add(4 * time.Second)
	writeChunk(t, lw, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"He\"}\n\n")
	count, sum := firstTokenState(t, model)
	if count != 1 || sum < 3.999 || sum > 4.001 {
		t.Fatalf("detector did not recover after oversized frame, got count=%d sum=%v", count, sum)
	}
}

func TestFirstTokenMeasuresFromArrival(t *testing.T) {
	// Routing and parsing time before dispatch counts toward first-token
	// latency (arrival zero point) but not toward the legacy dispatch TTFT.
	const model = "ft-arrival"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0.Add(300 * time.Millisecond) // dispatch 300ms after arrival
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	lw := &latencyWriter{
		ResponseWriter: rec,
		model:          model,
		enclave:        "enclave-1",
		pool:           "reserved",
		class:          "configured",
		arrival:        t0,
		start:          now,
		observeLegacy:  true,
		nowFunc:        func() time.Time { return now },
	}

	lw.WriteHeader(200)
	now = t0.Add(time.Second)
	writeChunk(t, lw, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")

	if _, sum := firstTokenState(t, model); sum < 0.999 || sum > 1.001 {
		t.Fatalf("first token must measure from arrival (want 1.0s), got %v", sum)
	}
	if _, sum := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "configured")); sum < 0.699 || sum > 0.701 {
		t.Fatalf("legacy TTFT must measure from dispatch (want 0.7s), got %v", sum)
	}
	if _, sum := histogramState(t, manager.ReplicaTTFTSeconds.WithLabelValues(model, "enclave-1", "reserved", "configured")); sum < 0.699 || sum > 0.701 {
		t.Fatalf("per-replica TTFT must measure from dispatch (want 0.7s), got %v", sum)
	}
}

func TestFirstTokenNonSSEBody(t *testing.T) {
	// A backend that answers a streaming request with a plain JSON body
	// delivers the whole generation at once: first byte is first token.
	const model = "ft-nonsse"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	lw := &latencyWriter{
		ResponseWriter: rec,
		model:          model,
		enclave:        "enclave-1",
		pool:           "reserved",
		class:          "configured",
		arrival:        t0,
		start:          t0,
		observeLegacy:  true,
		nowFunc:        func() time.Time { return now },
	}

	lw.WriteHeader(200)
	now = t0.Add(1200 * time.Millisecond)
	writeChunk(t, lw, `{"choices":[{"message":{"content":"Hello"}}]}`)

	count, sum := firstTokenState(t, model)
	if count != 1 || sum < 1.199 || sum > 1.201 {
		t.Fatalf("expected one first-token observation of 1.2s, got count=%d sum=%v", count, sum)
	}
}

func TestNoFirstTokenReasons(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	expiredCtx, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	for _, tc := range []struct {
		name   string
		reason string
		ctx    context.Context
		drive  func(t *testing.T, lw *latencyWriter)
	}{
		{
			name:   "client canceled before token",
			reason: "canceled",
			ctx:    canceledCtx,
			drive: func(t *testing.T, lw *latencyWriter) {
				lw.WriteHeader(200)
				writeChunk(t, lw, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
			},
		},
		{
			name:   "request deadline expired",
			reason: "timeout",
			ctx:    expiredCtx,
			drive: func(t *testing.T, lw *latencyWriter) {
				lw.WriteHeader(200)
			},
		},
		{
			name:   "backend HTTP error",
			reason: "error",
			ctx:    context.Background(),
			drive: func(t *testing.T, lw *latencyWriter) {
				lw.WriteHeader(502)
				writeChunk(t, lw, `{"error":{"message":"bad gateway"}}`)
			},
		},
		{
			name:   "in-stream error event",
			reason: "error",
			ctx:    context.Background(),
			drive: func(t *testing.T, lw *latencyWriter) {
				lw.WriteHeader(200)
				writeChunk(t, lw, "data: {\"error\":{\"message\":\"engine overloaded\",\"code\":503}}\n\n")
				writeChunk(t, lw, "data: [DONE]\n\n")
			},
		},
		{
			name:   "proxy aborted mid-stream",
			reason: "error",
			ctx:    context.Background(),
			drive: func(t *testing.T, lw *latencyWriter) {
				lw.WriteHeader(200)
				writeChunk(t, lw, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
				lw.aborted = true
			},
		},
		{
			name:   "stream ended with no output",
			reason: "no_output",
			ctx:    context.Background(),
			drive: func(t *testing.T, lw *latencyWriter) {
				lw.WriteHeader(200)
				writeChunk(t, lw, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"total_tokens\":10}}\n\n")
				writeChunk(t, lw, "data: [DONE]\n\n")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "ft-" + strings.ReplaceAll(tc.name, " ", "-")
			t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
			now := t0
			lw := newSSELatencyWriter(model, &now)
			tc.drive(t, lw)
			lw.finish(tc.ctx)

			if got := noFirstTokenCount(t, model, tc.reason); got != 1 {
				t.Fatalf("expected one %q count, got %v", tc.reason, got)
			}
			if count, _ := firstTokenState(t, model); count != 0 {
				t.Fatalf("token-less stream must not observe first-token latency, got count=%d", count)
			}
		})
	}
}

// TestFirstTokenThroughStreamingExtractor pins the integration with the
// tokencount extractor, which sits between the backend and the latency
// writer in the real proxy chain and re-chunks the SSE stream one line per
// write (with usage-only chunks filtered out). Detection must fire on the
// token event under that chunking, not on the control frames around it.
func TestFirstTokenThroughStreamingExtractor(t *testing.T) {
	const model = "ft-extractor"
	backendSSE := "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
		": keep-alive\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(backendSSE)),
	}
	newBody, _, err := tokencount.ExtractTokensFromResponseWithHandler(resp, model, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer newBody.Close()

	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	lw := newSSELatencyWriter(model, &now)
	lw.WriteHeader(200)

	// Copy the extractor's output the way the reverse proxy does — one
	// Write per Read — advancing the clock 10ms per chunk.
	var tokenLineAt time.Time
	buf := make([]byte, 32<<10)
	for {
		n, readErr := newBody.Read(buf)
		if n > 0 {
			now = now.Add(10 * time.Millisecond)
			if bytes.Contains(buf[:n], []byte("Hello")) {
				tokenLineAt = now
			}
			writeChunk(t, lw, string(buf[:n]))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	if tokenLineAt.IsZero() {
		t.Fatal("token line never reached the writer")
	}

	// The event terminator may land in the write after the token line, so
	// allow detection there, but never on an earlier (control) write and
	// never later.
	count, sum := firstTokenState(t, model)
	if count != 1 {
		t.Fatalf("expected one first-token observation, got %d", count)
	}
	earliest := tokenLineAt.Sub(t0).Seconds()
	latest := tokenLineAt.Add(10 * time.Millisecond).Sub(t0).Seconds()
	if sum < earliest-0.0001 || sum > latest+0.0001 {
		t.Fatalf("first token observed at %vs, want within [%v, %v]", sum, earliest, latest)
	}
}

func TestFirstTokenContentTypeParsing(t *testing.T) {
	// Media types are case-insensitive (RFC 9110 §8.3.1): every spelling of
	// text/event-stream must engage SSE token detection, so a control frame
	// is not counted as a token, while a type that merely embeds the string
	// must take the non-SSE path, where the first byte is the first token.
	const controlFrame = "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"
	for _, tc := range []struct {
		name        string
		contentType string
		sse         bool
	}{
		{"canonical-with-params", "text/event-stream; charset=utf-8", true},
		{"mixed-case", "Text/Event-Stream", true},
		{"upper-case-with-params", "TEXT/EVENT-STREAM;CHARSET=UTF-8", true},
		{"malformed-parameter", "text/event-stream; charset", true},
		{"embeds-the-substring", "text/event-stream-json", false},
		{"plain-json", "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "ft-ctype-" + tc.name
			t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
			now := t0
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Type", tc.contentType)
			lw := &latencyWriter{
				ResponseWriter: rec,
				model:          model,
				enclave:        "enclave-1",
				pool:           "reserved",
				class:          "configured",
				arrival:        t0,
				start:          t0,
				observeLegacy:  true,
				nowFunc:        func() time.Time { return now },
			}

			lw.WriteHeader(200)
			now = t0.Add(time.Second)
			writeChunk(t, lw, controlFrame)

			count, _ := firstTokenState(t, model)
			if tc.sse && count != 0 {
				t.Fatalf("%q: control frame counted as first token, detector not engaged", tc.contentType)
			}
			if !tc.sse && count != 1 {
				t.Fatalf("%q: non-SSE body must count its first byte as the first token, got count=%d", tc.contentType, count)
			}
		})
	}
}

// failingResponseWriter refuses all writes, like a connection whose client
// has gone away.
type failingResponseWriter struct{ header http.Header }

func newFailingResponseWriter() *failingResponseWriter {
	return &failingResponseWriter{header: make(http.Header)}
}

func (f *failingResponseWriter) Header() http.Header       { return f.header }
func (f *failingResponseWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }
func (f *failingResponseWriter) WriteHeader(int)           {}

func TestFirstTokenNotRecordedOnFailedWrite(t *testing.T) {
	const model = "ft-failed-write"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	fw := newFailingResponseWriter()
	fw.Header().Set("Content-Type", "text/event-stream")
	lw := &latencyWriter{
		ResponseWriter: fw,
		model:          model,
		enclave:        "enclave-1",
		pool:           "reserved",
		class:          "configured",
		arrival:        t0,
		start:          t0,
		observeLegacy:  true,
		nowFunc:        func() time.Time { return now },
	}

	lw.WriteHeader(200)
	now = t0.Add(time.Second)
	// The write carrying the token fails: the client never received it.
	if _, err := lw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")); err == nil {
		t.Fatal("expected the underlying write to fail")
	}
	if count, _ := firstTokenState(t, model); count != 0 {
		t.Fatalf("token on a failed write must not be recorded, got count=%d", count)
	}

	// The disconnect that failed the write also cancels the request
	// context; the request must land in the canceled bucket instead.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lw.finish(ctx)
	if got := noFirstTokenCount(t, model, "canceled"); got != 1 {
		t.Fatalf("expected one canceled count, got %v", got)
	}
}

func TestFirstTokenIgnoresEmptyWrite(t *testing.T) {
	// A zero-length write on a plain body delivers nothing and must not
	// stop the clock; the first non-empty write is the first token.
	const model = "ft-empty-write"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	lw := &latencyWriter{
		ResponseWriter: rec,
		model:          model,
		enclave:        "enclave-1",
		pool:           "reserved",
		class:          "configured",
		arrival:        t0,
		start:          t0,
		observeLegacy:  true,
		nowFunc:        func() time.Time { return now },
	}

	lw.WriteHeader(200)
	now = t0.Add(time.Second)
	writeChunk(t, lw, "")
	if count, _ := firstTokenState(t, model); count != 0 {
		t.Fatal("zero-length write counted as first token")
	}
	now = t0.Add(2 * time.Second)
	writeChunk(t, lw, `{"choices":[{"message":{"content":"Hello"}}]}`)
	count, sum := firstTokenState(t, model)
	if count != 1 || sum < 1.999 || sum > 2.001 {
		t.Fatalf("expected first token at the first non-empty write (2.0s), got count=%d sum=%v", count, sum)
	}
}

func TestToolLatencyWriterObservesFirstTokenOnly(t *testing.T) {
	// Tool-runtime streams must feed the first-token metrics under the
	// tool-runtime sentinel labels while leaving the dispatch-scoped legacy
	// TTFT / inter-token series untouched, so those keep their original
	// plain-proxy population.
	const model = "ft-tool-runtime"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	rec := httptest.NewRecorder()
	lw := newToolLatencyWriter(rec, t0, model, "configured")
	lw.nowFunc = func() time.Time { return now }

	// The tool streamers set the SSE content type themselves before the
	// first write; mirror that order here.
	rec.Header().Set("Content-Type", "text/event-stream")
	lw.WriteHeader(200)
	now = t0.Add(time.Second)
	writeChunk(t, lw, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
	now = t0.Add(3 * time.Second)
	writeChunk(t, lw, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<tinfoil-event>{\\\"status\\\":\\\"in_progress\\\"}</tinfoil-event>\"}}]}\n\n")

	count, sum := histogramState(t, manager.FirstTokenSeconds.WithLabelValues(model, "tool-runtime", "tool-runtime", "configured"))
	if count != 1 || sum < 2.999 || sum > 3.001 {
		t.Fatalf("expected one first-token observation of 3.0s under the tool-runtime sentinel, got count=%d sum=%v", count, sum)
	}

	if ttftCount, _ := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "configured")); ttftCount != 0 {
		t.Fatalf("tool-runtime stream must not feed legacy TTFT, got count=%d", ttftCount)
	}
	if ttftCount, _ := histogramState(t, manager.ReplicaTTFTSeconds.WithLabelValues(model, "tool-runtime", "tool-runtime", "configured")); ttftCount != 0 {
		t.Fatalf("tool-runtime stream must not feed per-replica TTFT, got count=%d", ttftCount)
	}
	if itlCount, _ := histogramState(t, manager.InterTokenSeconds.WithLabelValues(model, "configured")); itlCount != 0 {
		t.Fatalf("tool-runtime stream must not feed legacy inter-token gaps, got count=%d", itlCount)
	}
}

func TestToolLatencyWriterCountsTokenlessStream(t *testing.T) {
	const model = "ft-tool-canceled"
	t0 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	lw := newToolLatencyWriter(rec, t0, model, "none")
	lw.nowFunc = func() time.Time { return now }

	lw.WriteHeader(200)
	writeChunk(t, lw, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lw.finish(ctx)

	got := counterState(t, manager.NoFirstTokenTotal.WithLabelValues(model, "tool-runtime", "tool-runtime", "none", "canceled"))
	if got != 1 {
		t.Fatalf("expected one canceled count under the tool-runtime sentinel, got %v", got)
	}
}
