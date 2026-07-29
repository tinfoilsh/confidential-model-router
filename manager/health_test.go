package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func healthProbeCount(t *testing.T, model, host, result string) uint64 {
	t.Helper()
	observer := BackendHealthProbeDurationSeconds.WithLabelValues(model, host, result)
	metric, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatal("health probe histogram does not implement prometheus.Metric")
	}
	var value dto.Metric
	if err := metric.Write(&value); err != nil {
		t.Fatalf("write health probe metric: %v", err)
	}
	return value.GetHistogram().GetSampleCount()
}

func TestHealthProbeRecordsLatencyByResult(t *testing.T) {
	status := http.StatusNoContent
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/health" {
			t.Errorf("path = %s, want /health", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "https://")
	model := "health-probe-test-model"
	prober := newEnclaveHealthProber(host, model, "unused-in-test")
	prober.client = ts.Client()

	successBefore := healthProbeCount(t, model, host, "success")
	prober.probe(context.Background())
	if got := healthProbeCount(t, model, host, "success") - successBefore; got != 1 {
		t.Fatalf("success observations = %d, want 1", got)
	}

	status = http.StatusServiceUnavailable
	errorBefore := healthProbeCount(t, model, host, "error")
	prober.probe(context.Background())
	if got := healthProbeCount(t, model, host, "error") - errorBefore; got != 1 {
		t.Fatalf("error observations = %d, want 1", got)
	}
}

func TestHealthProbeDoesNotRecordShutdownCancellation(t *testing.T) {
	host := "shutdown-cancellation.invalid"
	model := "health-probe-cancellation-test-model"
	prober := newEnclaveHealthProber(host, model, "unused-in-test")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := healthProbeCount(t, model, host, "error")
	prober.probe(ctx)
	if got := healthProbeCount(t, model, host, "error") - before; got != 0 {
		t.Fatalf("error observations = %d, want 0 for shutdown cancellation", got)
	}
}
