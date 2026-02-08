package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/billing"
)

func setupTestProxyWithModel(t *testing.T, handler http.Handler, modelName string) (*httputil.ReverseProxy, *billing.Collector) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)

	backendURL, _ := url.Parse(backend.URL)
	collector := billing.NewCollector("")
	t.Cleanup(collector.Stop)

	proxy := newProxy(backendURL.Host, "", modelName, collector)
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

func TestProxyBilling_WebsearchEmitsTwoEvents(t *testing.T) {
	proxy, collector := setupTestProxyWithModel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":15,"total_tokens":20}}`))
	}), websearchModel)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-1234567890")
	req = req.WithContext(context.WithValue(req.Context(), RequestModelKey{}, "deepseek-r1-0528"))
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	events := collector.GetEvents()
	if len(events) != 2 {
		t.Fatalf("expected 2 billing events for websearch, got %d", len(events))
	}

	// First event: zero-token per-request event billed as "websearch"
	if events[0].PromptTokens != 0 || events[0].CompletionTokens != 0 || events[0].TotalTokens != 0 {
		t.Errorf("expected first event to be zero-token, got prompt=%d completion=%d total=%d",
			events[0].PromptTokens, events[0].CompletionTokens, events[0].TotalTokens)
	}
	if events[0].Model != websearchModel {
		t.Errorf("expected first event model %q, got %q", websearchModel, events[0].Model)
	}

	// Second event: token usage billed under the underlying model
	if events[1].PromptTokens != 5 || events[1].CompletionTokens != 15 || events[1].TotalTokens != 20 {
		t.Errorf("expected second event with tokens, got prompt=%d completion=%d total=%d",
			events[1].PromptTokens, events[1].CompletionTokens, events[1].TotalTokens)
	}
	if events[1].Model != "deepseek-r1-0528" {
		t.Errorf("expected second event model %q, got %q", "deepseek-r1-0528", events[1].Model)
	}
}
