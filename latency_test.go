package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

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
		class:          "configured",
		start:          now,
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
		class:          "none",
		start:          time.Now(),
	}

	lw.WriteHeader(502)
	if _, err := lw.Write([]byte(`{"error":{}}`)); err != nil {
		t.Fatal(err)
	}

	if count, _ := histogramState(t, manager.TTFTSeconds.WithLabelValues(model, "none")); count != 0 {
		t.Fatalf("error response must not observe TTFT, got count=%d", count)
	}
}

func TestLatencyWriterImplicit200(t *testing.T) {
	const model = "latency-test-implicit"
	lw := &latencyWriter{
		ResponseWriter: httptest.NewRecorder(),
		model:          model,
		class:          "none",
		start:          time.Now(),
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
	lw := newLatencyWriter(rec, "latency-test-flush", "none")
	lw.Flush()
	if !rec.Flushed {
		t.Fatal("Flush was not forwarded to the underlying writer")
	}
}
