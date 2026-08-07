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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

func setupTestProxyWithModel(t *testing.T, handler http.Handler, modelName string) *httputil.ReverseProxy {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)

	backendURL, _ := url.Parse(backend.URL)
	proxy := newProxy(backendURL.Host, "", modelName, nil, newCircuitBreaker(), "")
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = backendURL.Scheme
		req.URL.Host = backendURL.Host
	}
	proxy.Transport = http.DefaultTransport

	return proxy
}

func newEnabledTestCollector(t *testing.T) *billing.Collector {
	t.Helper()
	collector, err := billing.NewCollector("https://unused.invalid", "router-test", "test-reporter-secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(collector.Stop)
	return collector
}

func setupTestProxy(t *testing.T, handler http.Handler) *httputil.ReverseProxy {
	return setupTestProxyWithModel(t, handler, "test-model")
}

// TestProxyDirector_RewritesHostHeader ensures the outbound Host header is
// set to the configured enclave host (not the inbound request's Host). Without
// this rewrite, subdomain-dispatching enclaves (e.g. confidential-realtime-models)
// receive Host: <router-public-host> via X-Forwarded-Host and 404.
func TestProxyDirector_RewritesHostHeader(t *testing.T) {
	const enclaveHost = "voxtral-tts.realtime.inf9.tinfoil.sh"

	proxy := newProxy(enclaveHost, "", "voxtral-tts", nil, newCircuitBreaker(), "")

	req := httptest.NewRequest("POST", "/v1/audio/speech", nil)
	req.Host = "inference.tinfoil.sh"
	req.URL.Scheme = "" // mimic the inbound request: scheme/host empty before director runs

	proxy.Director(req)

	if req.Host != enclaveHost {
		t.Fatalf("req.Host = %q, want %q (Host header must match target so subdomain enclaves can dispatch)", req.Host, enclaveHost)
	}
	if req.URL.Host != enclaveHost {
		t.Fatalf("req.URL.Host = %q, want %q", req.URL.Host, enclaveHost)
	}
	if req.URL.Scheme != "https" {
		t.Fatalf("req.URL.Scheme = %q, want %q", req.URL.Scheme, "https")
	}
}

// TestProxyDirector_SignsUsageContext verifies that the Director signs a
// usage-context header declaring BillCustomerRequest=false when a
// usageContextSecret is configured, so the downstream shim suppresses its own
// billing event on the cloud path (router → shim).
func TestProxyDirector_SignsUsageContext(t *testing.T) {
	const enclaveHost = "glm-5-2.inf9.tinfoil.sh"
	const secret = "test-usage-context-secret-32-bytes-long!"
	const apiKey = "sk-test-key-1234567890"

	collector := newEnabledTestCollector(t)

	proxy := newProxy(enclaveHost, "", "glm-5-2", collector, newCircuitBreaker(), secret)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// A custom client cannot choose the suppression context. The router owns
	// both headers and must replace any values received at ingress.
	req.Header.Set(usagereporting.HeaderContext, "client-supplied")
	req.Header.Set(usagereporting.HeaderUsageContextSignature, "client-supplied")
	req.URL.Scheme = ""

	proxy.Director(req)

	ctx, present, err := usagereporting.FromHeaders(req.Header, secret, time.Now(), time.Minute)
	if !present {
		t.Fatalf("usage-context header not present after Director")
	}
	if err != nil {
		t.Fatalf("usage-context verification failed: %v", err)
	}
	if ctx.BillCustomerRequest {
		t.Fatalf("BillCustomerRequest = true, want false (router already counted)")
	}
	if ctx.ParentService != usagereporting.ServiceRouter {
		t.Fatalf("ParentService = %q, want %q", ctx.ParentService, usagereporting.ServiceRouter)
	}
	if ctx.Depth != 1 {
		t.Fatalf("Depth = %d, want 1", ctx.Depth)
	}
	if !usagereporting.VerifyAPIKeyHash(apiKey, ctx.APIKeyHash) {
		t.Fatalf("APIKeyHash does not match the request's bearer token")
	}
}

// TestProxyDirector_NoUsageContextWithoutSecret verifies that no
// usage-context header is set when the secret is empty (fail-open for
// inference; the shim bills normally).
func TestProxyDirector_NoUsageContextWithoutSecret(t *testing.T) {
	const enclaveHost = "glm-5-2.inf9.tinfoil.sh"

	collector := newEnabledTestCollector(t)

	proxy := newProxy(enclaveHost, "", "glm-5-2", collector, newCircuitBreaker(), "")

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set(usagereporting.HeaderContext, "client-supplied")
	req.Header.Set(usagereporting.HeaderUsageContextSignature, "client-supplied")
	req.URL.Scheme = ""

	proxy.Director(req)

	if h := req.Header.Get(usagereporting.HeaderContext); h != "" {
		t.Fatalf("usage-context header should be absent without secret, got %q", h)
	}
	if h := req.Header.Get(usagereporting.HeaderUsageContextSignature); h != "" {
		t.Fatalf("usage-context signature should be absent without secret, got %q", h)
	}
}

// TestProxyDirector_NoUsageContextWithoutCollector verifies that a proxy with
// no billing responsibility leaves the downstream shim responsible for it.
func TestProxyDirector_NoUsageContextWithoutCollector(t *testing.T) {
	const enclaveHost = "glm-5-2.inf9.tinfoil.sh"
	const secret = "test-usage-context-secret-32-bytes-long!"

	proxy := newProxy(enclaveHost, "", "glm-5-2", nil, newCircuitBreaker(), secret)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set(usagereporting.HeaderContext, "client-supplied")
	req.Header.Set(usagereporting.HeaderUsageContextSignature, "client-supplied")
	req.URL.Scheme = ""
	proxy.Director(req)

	if h := req.Header.Get(usagereporting.HeaderContext); h != "" {
		t.Fatalf("usage-context header should be absent without a collector, got %q", h)
	}
	if h := req.Header.Get(usagereporting.HeaderUsageContextSignature); h != "" {
		t.Fatalf("usage-context signature should be absent without a collector, got %q", h)
	}
}

// TestProxyDirector_NoUsageContextWithoutAuth verifies that the Director does
// not mint an unbound suppression context when there is no API key.
func TestProxyDirector_NoUsageContextWithoutAuth(t *testing.T) {
	const enclaveHost = "glm-5-2.inf9.tinfoil.sh"
	const secret = "test-usage-context-secret-32-bytes-long!"

	collector := newEnabledTestCollector(t)

	proxy := newProxy(enclaveHost, "", "glm-5-2", collector, newCircuitBreaker(), secret)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set(usagereporting.HeaderContext, "client-supplied")
	req.Header.Set(usagereporting.HeaderUsageContextSignature, "client-supplied")
	req.URL.Scheme = ""

	proxy.Director(req)

	if h := req.Header.Get(usagereporting.HeaderContext); h != "" {
		t.Fatalf("usage-context header should be absent without auth, got %q", h)
	}
	if h := req.Header.Get(usagereporting.HeaderUsageContextSignature); h != "" {
		t.Fatalf("usage-context signature should be absent without auth, got %q", h)
	}
}

func TestProxyUsageMetrics_IncludesCachedTokensForAllModels(t *testing.T) {
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
	want := "prompt=69,completion=20,total=89,cached_prompt_tokens=64,uncached_prompt_tokens=5,model=test-model"
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

func TestCircuitBreaker_ClaimProbeAfterCooldown(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	if _, ok := cb.ClaimProbe(); ok {
		t.Fatal("should not probe before cooldown")
	}
	// Simulate cooldown by backdating lastFailureNano
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())

	if _, ok := cb.ClaimProbe(); !ok {
		t.Fatal("expected probe after cooldown")
	}
	if cb.State() != cbHalfOpen {
		t.Fatalf("expected half-open, got %d", cb.State())
	}
	// Second call should return false (only one probe allowed)
	if _, ok := cb.ClaimProbe(); ok {
		t.Fatal("expected no second probe while half-open")
	}
}

func TestCircuitBreaker_HalfOpenToClosedOnSuccess(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	cb.ClaimProbe() // transition to half-open

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
	cb.ClaimProbe() // transition to half-open

	cb.RecordFailure()
	if cb.State() != cbOpen {
		t.Fatalf("expected open after half-open failure, got %d", cb.State())
	}
}

func TestCircuitBreaker_AbortProbeReturnsToOpen(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	token, ok := cb.ClaimProbe()
	if !ok {
		t.Fatal("expected probe after cooldown")
	}
	failures := cb.ConsecutiveFailures()

	if cb.AbortProbe(token - 1) {
		t.Fatal("a stale token must not abort the current probe")
	}
	if !cb.AbortProbe(token) {
		t.Fatal("expected claimed probe to abort")
	}
	if cb.State() != cbOpen {
		t.Fatalf("expected open after abort, got %d", cb.State())
	}
	if cb.ConsecutiveFailures() != failures {
		t.Fatalf("abort changed failure count: got %d, want %d", cb.ConsecutiveFailures(), failures)
	}
	if _, ok := cb.ClaimProbe(); ok {
		t.Fatal("should restart cooldown after abort")
	}
}

// TestProxyCancellationReleasesRecoveryProbe pins that a client
// cancellation on the public proxy path returns a claimed recovery probe
// to open instead of stranding the breaker half-open forever — but only
// when the cancelled request owns the claim.
func TestProxyCancellationReleasesRecoveryProbe(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < cbFailureThreshold; i++ {
		cb.RecordFailure()
	}
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	token, ok := cb.ClaimProbe()
	if !ok {
		t.Fatal("expected probe claim after cooldown")
	}
	claim := &ProbeClaim{cb: cb, token: token, modelName: "probe-model", host: "probe-host.test"}

	proxy := newProxy("probe-host.test", "", "probe-model", nil, cb, "")

	// A cancelled request that does not own the claim must leave the
	// in-flight probe alone.
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	proxy.ErrorHandler(rec, req, context.Canceled)
	if cb.State() != cbHalfOpen {
		t.Fatalf("breaker state = %d, want half-open (probe not owned by canceller)", cb.State())
	}

	// The owning request's cancellation releases it.
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req = req.WithContext(WithProbeClaim(req.Context(), claim))
	rec = httptest.NewRecorder()
	proxy.ErrorHandler(rec, req, context.Canceled)

	if cb.State() != cbOpen {
		t.Fatalf("breaker state = %d, want open after cancelled probe", cb.State())
	}
	if _, ok := cb.ClaimProbe(); ok {
		t.Fatal("cooldown must restart after a cancelled probe")
	}
}

// TestProxyOversizedBodyIsClientError pins that a MaxBytesReader trip
// during the outbound copy answers 413 and records no backend failure, so
// cheap oversized uploads cannot open a healthy enclave's breaker.
func TestProxyOversizedBodyIsClientError(t *testing.T) {
	cb := newCircuitBreaker()
	proxy := newProxy("oversize-host.test", "", "oversize-model", nil, cb, "")

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", nil)
	rec := httptest.NewRecorder()
	proxy.ErrorHandler(rec, req, &http.MaxBytesError{Limit: 1})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if got := cb.ConsecutiveFailures(); got != 0 {
		t.Fatalf("breaker failures = %d, want 0 (client fault)", got)
	}
	if cb.State() != cbClosed {
		t.Fatalf("breaker state = %d, want closed", cb.State())
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

func tokenHistogramState(t *testing.T, o prometheus.Observer) (count uint64, sum float64) {
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

func tokenCounterState(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	pb := &dto.Metric{}
	if err := c.Write(pb); err != nil {
		t.Fatalf("failed to read counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

func TestObserveTokenUsage(t *testing.T) {
	const model = "token-usage-test"
	ctx := WithTokenMetricLabels(context.Background(), "reserved", "configured")
	observeTokenUsage(ctx, model, true, &tokencount.Usage{
		PromptTokens:        1200,
		CompletionTokens:    340,
		PromptTokensDetails: &tokencount.PromptTokensDetails{CachedTokens: 900},
	})

	count, sum := tokenHistogramState(t, RequestPromptTokens.WithLabelValues(model, "reserved", "configured", "streaming"))
	if count != 1 || sum != 1200 {
		t.Errorf("prompt histogram: got count=%d sum=%v, want 1/1200", count, sum)
	}
	count, sum = tokenHistogramState(t, RequestCompletionTokens.WithLabelValues(model, "reserved", "configured", "streaming"))
	if count != 1 || sum != 340 {
		t.Errorf("completion histogram: got count=%d sum=%v, want 1/340", count, sum)
	}

	if got := tokenCounterState(t, RequestCachedPromptTokensTotal.WithLabelValues(model, "reserved", "configured", "streaming")); got != 900 {
		t.Errorf("cached counter: got %v, want 900", got)
	}

	// Non-streaming lands on its own series; usage without cached-token
	// details counts as fully uncached.
	observeTokenUsage(ctx, model, false, &tokencount.Usage{PromptTokens: 80, CompletionTokens: 20})
	count, sum = tokenHistogramState(t, RequestPromptTokens.WithLabelValues(model, "reserved", "configured", "non_streaming"))
	if count != 1 || sum != 80 {
		t.Errorf("non-streaming prompt histogram: got count=%d sum=%v, want 1/80", count, sum)
	}
	if got := tokenCounterState(t, RequestCachedPromptTokensTotal.WithLabelValues(model, "reserved", "configured", "non_streaming")); got != 0 {
		t.Errorf("non-streaming cached counter: got %v, want 0", got)
	}

	// A context without labels must not observe anything.
	observeTokenUsage(context.Background(), model, true, &tokencount.Usage{PromptTokens: 999})
	count, _ = tokenHistogramState(t, RequestPromptTokens.WithLabelValues(model, "reserved", "configured", "streaming"))
	if count != 1 {
		t.Errorf("unlabeled context observed: count=%d, want 1", count)
	}
}
