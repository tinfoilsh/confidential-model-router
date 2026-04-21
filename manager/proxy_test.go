package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/billing"
)

func setupTestProxyWithModel(t *testing.T, handler http.Handler, modelName string) *httputil.ReverseProxy {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)

	backendURL, _ := url.Parse(backend.URL)
	collector := billing.NewCollector("", "", "")
	t.Cleanup(collector.Stop)

	proxy := newProxy(backendURL.Host, "", modelName, collector, newCircuitBreaker())
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = backendURL.Scheme
		req.URL.Host = backendURL.Host
	}
	proxy.Transport = http.DefaultTransport

	return proxy
}

func setupTestProxy(t *testing.T, handler http.Handler) *httputil.ReverseProxy {
	return setupTestProxyWithModel(t, handler, "test-model")
}

func TestProxyUsageMetrics_NonKimiModelKeepsLegacyUsageFormat(t *testing.T) {
	proxy := setupTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":69,"completion_tokens":20,"total_tokens":89,"prompt_tokens_details":{"cached_tokens":64}}}`))
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	req.Header.Set(UsageMetricsRequestHeader, "true")
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}
	ctx := context.WithValue(req.Context(), usageWriterKey{}, wrapper)

	proxy.ServeHTTP(wrapper, req.WithContext(ctx))

	got := rec.Header().Get(UsageMetricsResponseHeader)
	want := "prompt=69,completion=20,total=89"
	if got != want {
		t.Fatalf("usage header = %q, want %q", got, want)
	}
}

// --- Circuit breaker tests ---

func TestCircuitBreaker_StartsClosed(t *testing.T) {
	cb := newCircuitBreaker()
	if cb.State() != cbClosed {
		t.Fatalf("expected closed, got %d", cb.State())
	}
	if !cb.Closed() {
		t.Fatal("expected closed")
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold-1; i++ {
		cb.RecordFailure()
		if cb.State() != cbClosed {
			t.Fatalf("expected closed after %d failures, got %d", i+1, cb.State())
		}
	}
	cb.RecordFailure()
	if cb.State() != cbOpen {
		t.Fatalf("expected open after %d failures, got %d", cbFailureThreshold, cb.State())
	}
	if cb.Closed() {
		t.Fatal("expected not closed when open")
	}
}

func TestCircuitBreaker_SuccessResetsClosed(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	if cb.State() != cbOpen {
		t.Fatal("expected open")
	}
	cb.RecordSuccess()
	if cb.State() != cbClosed {
		t.Fatalf("expected closed after success, got %d", cb.State())
	}
	if cb.ConsecutiveFailures() != 0 {
		t.Fatalf("expected 0 failures after success, got %d", cb.ConsecutiveFailures())
	}
}

func TestCircuitBreaker_NeedProbeAfterCooldown(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	if cb.NeedProbe() {
		t.Fatal("should not probe before cooldown")
	}
	// Simulate cooldown by backdating lastFailureNano
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())

	if !cb.NeedProbe() {
		t.Fatal("expected probe after cooldown")
	}
	if cb.State() != cbHalfOpen {
		t.Fatalf("expected half-open, got %d", cb.State())
	}
	// Second call should return false (only one probe allowed)
	if cb.NeedProbe() {
		t.Fatal("expected no second probe while half-open")
	}
}

func TestCircuitBreaker_HalfOpenToClosedOnSuccess(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	cb.NeedProbe() // transition to half-open

	cb.RecordSuccess()
	if cb.State() != cbClosed {
		t.Fatalf("expected closed after half-open success, got %d", cb.State())
	}
}

func TestCircuitBreaker_HalfOpenToOpenOnFailure(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	cb.NeedProbe() // transition to half-open

	cb.RecordFailure()
	if cb.State() != cbOpen {
		t.Fatalf("expected open after half-open failure, got %d", cb.State())
	}
}

func TestCircuitBreaker_SuccessResetsFailureCount(t *testing.T) {
	cb := newCircuitBreaker()
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.ConsecutiveFailures() != 2 {
		t.Fatalf("expected 2 failures, got %d", cb.ConsecutiveFailures())
	}
	cb.RecordSuccess()
	if cb.ConsecutiveFailures() != 0 {
		t.Fatalf("expected 0 failures after success, got %d", cb.ConsecutiveFailures())
	}
	// Verify it takes full threshold again to trip
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	if cb.State() != cbOpen {
		t.Fatal("expected open after fresh threshold failures")
	}
}

// --- Slow header tripper tests ---

func TestSlowHeaderTripper_FastResponse_NoCallback(t *testing.T) {
	var called atomic.Bool
	tripper := &slowHeaderTripper{
		base:    http.DefaultTransport,
		timeout: 100 * time.Millisecond,
		onSlow: func() {
			called.Store(true)
		},
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	req, _ := http.NewRequest("GET", backend.URL, nil)
	resp, err := tripper.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	// Give a bit of time to ensure callback wasn't called
	time.Sleep(150 * time.Millisecond)
	if called.Load() {
		t.Fatal("onSlow should not be called for fast responses")
	}
}

func TestSlowHeaderTripper_SlowResponse_CallbackFired(t *testing.T) {
	called := make(chan struct{}, 1)
	tripper := &slowHeaderTripper{
		base:    http.DefaultTransport,
		timeout: 50 * time.Millisecond,
		onSlow: func() {
			called <- struct{}{}
		},
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	req, _ := http.NewRequest("GET", backend.URL, nil)
	resp, err := tripper.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onSlow should be called for slow responses")
	}
}

func TestSlowHeaderTripper_SlowResponse_RequestNotKilled(t *testing.T) {
	tripper := &slowHeaderTripper{
		base:    http.DefaultTransport,
		timeout: 50 * time.Millisecond,
		onSlow:  func() {},
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	req, _ := http.NewRequest("GET", backend.URL, nil)
	resp, err := tripper.RoundTrip(req)
	if err != nil {
		t.Fatalf("request was killed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
