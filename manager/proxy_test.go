package manager

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
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
	collector := billing.NewCollector("", "", "")
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

// TestProxyBilling_WebsearchEmitsPerRequestFeeEvent pins the
// double-billing guard for the `websearch` model: the websearch service
// proxies its own upstream calls through this same proxy under the
// user's API key, so those calls get token-billed there directly.
// Emitting a usage-based billing event at THIS layer too would
// double-charge the user, so the websearch model is special-cased to
// emit only a per-request fee event (all token fields zero) even when
// the backend returned real usage numbers.
func TestProxyBilling_WebsearchEmitsPerRequestFeeEvent(t *testing.T) {
	proxy, collector := setupTestProxyWithModel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":15,"total_tokens":20}}`))
	}), "websearch")

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event for websearch, got %d", len(events))
	}

	if events[0].PromptTokens != 0 || events[0].CompletionTokens != 0 || events[0].TotalTokens != 0 {
		t.Errorf("websearch model must emit zero-token fee event to avoid double-billing, got prompt=%d completion=%d total=%d",
			events[0].PromptTokens, events[0].CompletionTokens, events[0].TotalTokens)
	}
	if events[0].Model != "websearch" {
		t.Errorf("expected event model %q, got %q", "websearch", events[0].Model)
	}
}

func TestProxyBilling_ClientHeaderCannotBypassBilling(t *testing.T) {
	proxy, collector := setupTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":15,"total_tokens":20}}`))
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	req.Header.Set("X-Tinfoil-Internal-Tool-Loop", "true")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event even with legacy internal header, got %d", len(events))
	}
	if events[0].PromptTokens != 5 || events[0].CompletionTokens != 15 {
		t.Errorf("expected token event preserved, got prompt=%d completion=%d",
			events[0].PromptTokens, events[0].CompletionTokens)
	}
}

func TestProxyUsageMetrics_NonStreamingIncludesCachedPromptBreakdown(t *testing.T) {
	proxy, collector := setupTestProxyWithModel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":69,"completion_tokens":20,"total_tokens":89,"prompt_tokens_details":{"cached_tokens":64}}}`))
	}), promptTokenDetailsMetricsModel)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	req.Header.Set(UsageMetricsRequestHeader, "true")
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}
	ctx := context.WithValue(req.Context(), usageWriterKey{}, wrapper)

	proxy.ServeHTTP(wrapper, req.WithContext(ctx))

	got := rec.Header().Get(UsageMetricsResponseHeader)
	want := "prompt=69,completion=20,total=89,cached_prompt_tokens=64,uncached_prompt_tokens=5"
	if got != want {
		t.Fatalf("usage header = %q, want %q", got, want)
	}

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event, got %d", len(events))
	}
	if events[0].PromptTokens != 69 {
		t.Fatalf("prompt_tokens billed = %d, want 69", events[0].PromptTokens)
	}
	if events[0].CompletionTokens != 20 {
		t.Fatalf("completion_tokens billed = %d, want 20", events[0].CompletionTokens)
	}
	if events[0].TotalTokens != 89 {
		t.Fatalf("total_tokens billed = %d, want 89", events[0].TotalTokens)
	}
}

func TestProxyUsageMetrics_StreamingIncludesCachedPromptBreakdown(t *testing.T) {
	proxy, collector := setupTestProxyWithModel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"index\":0}],\"usage\":{\"prompt_tokens\":69,\"completion_tokens\":1,\"total_tokens\":70}}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":69,\"completion_tokens\":20,\"total_tokens\":89,\"prompt_tokens_details\":{\"cached_tokens\":64}}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}), promptTokenDetailsMetricsModel)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	req.Header.Set(UsageMetricsRequestHeader, "true")
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}
	ctx := context.WithValue(req.Context(), usageWriterKey{}, wrapper)

	proxy.ServeHTTP(wrapper, req.WithContext(ctx))
	if wrapper.TrailerEnabled() {
		wrapper.WriteTrailer()
	}

	got := rec.Header().Get(UsageMetricsResponseHeader)
	want := "prompt=69,completion=20,total=89,cached_prompt_tokens=64,uncached_prompt_tokens=5"
	if got != want {
		t.Fatalf("usage trailer = %q, want %q", got, want)
	}

	body := rec.Body.String()
	if strings.Contains(body, "\"choices\":[]") {
		t.Fatal("usage-only chunk should be filtered when client did not explicitly request usage chunks")
	}

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event, got %d", len(events))
	}
	if events[0].PromptTokens != 69 {
		t.Fatalf("prompt_tokens billed = %d, want 69", events[0].PromptTokens)
	}
	if events[0].CompletionTokens != 20 {
		t.Fatalf("completion_tokens billed = %d, want 20", events[0].CompletionTokens)
	}
	if events[0].TotalTokens != 89 {
		t.Fatalf("total_tokens billed = %d, want 89", events[0].TotalTokens)
	}
}

func TestProxyUsageMetrics_StreamingResponsesAPIIncludesCachedPromptBreakdown(t *testing.T) {
	proxy, collector := setupTestProxyWithModel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: response.created\n")
		io.WriteString(w, "data: {\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"usage\":null},\"type\":\"response.created\"}\n\n")
		io.WriteString(w, "event: response.completed\n")
		io.WriteString(w, "data: {\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":69,\"input_tokens_details\":{\"cached_tokens\":64},\"output_tokens\":20,\"total_tokens\":89}},\"type\":\"response.completed\"}\n\n")
	}), promptTokenDetailsMetricsModel)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	req.Header.Set(UsageMetricsRequestHeader, "true")
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}
	ctx := context.WithValue(req.Context(), usageWriterKey{}, wrapper)

	proxy.ServeHTTP(wrapper, req.WithContext(ctx))
	if wrapper.TrailerEnabled() {
		wrapper.WriteTrailer()
	}

	got := rec.Header().Get(UsageMetricsResponseHeader)
	want := "prompt=69,completion=20,total=89,cached_prompt_tokens=64,uncached_prompt_tokens=5"
	if got != want {
		t.Fatalf("usage trailer = %q, want %q", got, want)
	}

	events := collector.GetEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 billing event, got %d", len(events))
	}
	if events[0].PromptTokens != 69 {
		t.Fatalf("prompt_tokens billed = %d, want 69", events[0].PromptTokens)
	}
	if events[0].CompletionTokens != 20 {
		t.Fatalf("completion_tokens billed = %d, want 20", events[0].CompletionTokens)
	}
	if events[0].TotalTokens != 89 {
		t.Fatalf("total_tokens billed = %d, want 89", events[0].TotalTokens)
	}
}

func TestProxyUsageMetrics_NonKimiModelKeepsLegacyUsageFormat(t *testing.T) {
	proxy, _ := setupTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
