package manager

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/billing"
)

func setupTestProxyWithModel(t *testing.T, handler http.Handler, modelName string) (*httputil.ReverseProxy, *billing.Collector) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)

	backendURL, _ := url.Parse(backend.URL)
	collector := billing.NewCollector("")
	t.Cleanup(collector.Stop)

	proxy := newProxy(backendURL.Host, "", modelName, collector, newCircuitBreaker())
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = backendURL.Scheme
		req.URL.Host = backendURL.Host
	}
	proxy.Transport = http.DefaultTransport

	return proxy, collector
}

func setupTestProxy(t *testing.T, handler http.Handler) (*httputil.ReverseProxy, *billing.Collector) {
	return setupTestProxyWithModel(t, handler, "test-model")
}

func TestProxyBilling_NonJSONEmitsEvent(t *testing.T) {
	proxy, collector := setupTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("binary document data"))
	}))

	req := httptest.NewRequest("POST", "/v1/documents", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event for non-JSON response, got %d", len(events))
	}
	e := events[0]
	if e.PromptTokens != 0 || e.CompletionTokens != 0 || e.TotalTokens != 0 {
		t.Errorf("expected zero-token event, got prompt=%d completion=%d total=%d",
			e.PromptTokens, e.CompletionTokens, e.TotalTokens)
	}
	if e.Model != "test-model" {
		t.Errorf("expected model %q, got %q", "test-model", e.Model)
	}
	if e.APIKey != "test-key-1234567890" {
		t.Errorf("expected api key %q, got %q", "test-key-1234567890", e.APIKey)
	}
}

func TestProxyBilling_JSONWithUsageEmitsSingleEvent(t *testing.T) {
	proxy, collector := setupTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 billing event (no double-emit), got %d", len(events))
	}
	e := events[0]
	if e.PromptTokens != 10 {
		t.Errorf("expected prompt_tokens=10, got %d", e.PromptTokens)
	}
	if e.CompletionTokens != 20 {
		t.Errorf("expected completion_tokens=20, got %d", e.CompletionTokens)
	}
	if e.TotalTokens != 30 {
		t.Errorf("expected total_tokens=30, got %d", e.TotalTokens)
	}
}

func TestProxyBilling_JSONWithoutUsageEmitsEvent(t *testing.T) {
	proxy, collector := setupTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"text":"hello world"}`))
	}))

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event for JSON without usage, got %d", len(events))
	}
	e := events[0]
	if e.PromptTokens != 0 || e.CompletionTokens != 0 || e.TotalTokens != 0 {
		t.Errorf("expected zero-token event, got prompt=%d completion=%d total=%d",
			e.PromptTokens, e.CompletionTokens, e.TotalTokens)
	}
}

func TestProxyBilling_ErrorResponseNoEvent(t *testing.T) {
	proxy, collector := setupTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal server error"}`))
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 0 {
		t.Fatalf("expected 0 billing events for error response, got %d", len(events))
	}
}

func TestProxyBilling_WebsearchEmitsOnlyZeroTokenEvent(t *testing.T) {
	proxy, collector := setupTestProxyWithModel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":15,"total_tokens":20}}`))
	}), websearchModel)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event for websearch (zero-token only), got %d", len(events))
	}

	// Only the zero-token per-request event billed as "websearch".
	// Token usage is billed when the responder calls the underlying
	// model through the proxy with the user's API key.
	if events[0].PromptTokens != 0 || events[0].CompletionTokens != 0 || events[0].TotalTokens != 0 {
		t.Errorf("expected zero-token event, got prompt=%d completion=%d total=%d",
			events[0].PromptTokens, events[0].CompletionTokens, events[0].TotalTokens)
	}
	if events[0].Model != websearchModel {
		t.Errorf("expected event model %q, got %q", websearchModel, events[0].Model)
	}
}

// --- Circuit breaker tests ---

func TestCircuitBreaker_StartsClosedAndAvailable(t *testing.T) {
	cb := newCircuitBreaker()
	if cb.State() != cbClosed {
		t.Fatalf("expected closed, got %d", cb.State())
	}
	if !cb.Available() {
		t.Fatal("expected available")
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
	if cb.Available() {
		t.Fatal("expected unavailable when open")
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

func TestCircuitBreaker_HalfOpenAfterCooldown(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	// Simulate cooldown by backdating lastFailureNano
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())

	if !cb.Available() {
		t.Fatal("expected available after cooldown")
	}
	if cb.State() != cbHalfOpen {
		t.Fatalf("expected half-open, got %d", cb.State())
	}
	// Second call should return false (half-open allows only one probe)
	if cb.Available() {
		t.Fatal("expected unavailable while half-open")
	}
}

func TestCircuitBreaker_HalfOpenToClosedOnSuccess(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	cb.Available() // transition to half-open

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
	cb.Available() // transition to half-open

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
