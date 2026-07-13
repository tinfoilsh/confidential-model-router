package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLocalMCPEndpointEnvVar pins the exact shape of the debug-bypass
// env var name derived from a model name. The router advertises this
// contract to operators setting up local tool-server development, so
// any change here is a user-visible API break: new MCP servers must
// be able to predict their override env var from their model name.
func TestLocalMCPEndpointEnvVar(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"websearch", "LOCAL_MCP_ENDPOINT_WEBSEARCH"},
		{"code-runner", "LOCAL_MCP_ENDPOINT_CODE_RUNNER"},
		{"file-search-v2", "LOCAL_MCP_ENDPOINT_FILE_SEARCH_V2"},
		{"upper-CASE", "LOCAL_MCP_ENDPOINT_UPPER_CASE"},
		{"with space", "LOCAL_MCP_ENDPOINT_WITH_SPACE"},
	}
	for _, tc := range cases {
		if got := localMCPEndpointEnvVar(tc.model); got != tc.want {
			t.Errorf("localMCPEndpointEnvVar(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// TestPostToEnclaveInflight pins that internal dispatches are visible to
// least-loaded selection: in flight from dispatch until the response body is
// closed, decremented exactly once, and released on transport errors.
func TestPostToEnclaveInflight(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	// Cleanups run LIFO: the handler must be released before ts.Close
	// waits on it, or a failed assertion deadlocks the test binary.
	t.Cleanup(ts.Close)
	t.Cleanup(releaseFn)
	e := &Enclave{
		host:      strings.TrimPrefix(ts.URL, "https://"),
		modelName: "test-model",
		cb:        newCircuitBreaker(),
	}

	resp, err := postToEnclave(context.Background(), ts.Client(), e, "/v1/chat/completions", []byte("{}"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.inflight.Load(); got != 1 {
		t.Fatalf("in-flight after headers = %d, want 1 (body still open)", got)
	}
	if got := e.cb.ConsecutiveFailures(); got != 0 {
		t.Fatalf("breaker failures after success = %d, want 0", got)
	}
	releaseFn()
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := e.inflight.Load(); got != 0 {
		t.Fatalf("in-flight after body close = %d, want 0", got)
	}
	// Double-close must not double-decrement.
	resp.Body.Close()
	if got := e.inflight.Load(); got != 0 {
		t.Fatalf("in-flight after double close = %d, want 0", got)
	}

	// Transport error: released immediately.
	ts2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := ts2.Client()
	ts2.Close()
	e2 := &Enclave{
		host:      strings.TrimPrefix(ts2.URL, "https://"),
		modelName: "test-model",
		cb:        newCircuitBreaker(),
	}
	if _, err := postToEnclave(context.Background(), client, e2, "/x", nil, nil, nil); err == nil {
		t.Fatal("expected transport error against closed server")
	}
	if got := e2.inflight.Load(); got != 0 {
		t.Fatalf("in-flight after error = %d, want 0", got)
	}
	if got := e2.cb.ConsecutiveFailures(); got != 1 {
		t.Fatalf("breaker failures after transport error = %d, want 1", got)
	}
}

func TestPostToEnclaveCancellationReleasesRecoveryProbe(t *testing.T) {
	cb := newCircuitBreaker()
	cb.consecutiveFailures.Store(cbFailureThreshold)
	cb.storeState(cbOpen)
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	e := &Enclave{
		host:      "example.invalid",
		modelName: "test-model",
		cb:        cb,
	}
	claim := e.claimProbe()
	if claim == nil {
		t.Fatal("expected probe claim after cooldown")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := postToEnclave(ctx, http.DefaultClient, e, "/x", nil, nil, claim); err == nil {
		t.Fatal("expected canceled request to fail")
	}
	if got := cb.State(); got != cbOpen {
		t.Fatalf("breaker state = %d, want open", got)
	}
	if got := cb.ConsecutiveFailures(); got != cbFailureThreshold {
		t.Fatalf("breaker failures = %d, want %d", got, cbFailureThreshold)
	}
}

// TestPostToEnclaveNonOwnerCancellationKeepsProbe pins that a cancelled
// dispatch without the claim cannot release someone else's in-flight probe.
func TestPostToEnclaveNonOwnerCancellationKeepsProbe(t *testing.T) {
	cb := newCircuitBreaker()
	cb.consecutiveFailures.Store(cbFailureThreshold)
	cb.storeState(cbOpen)
	cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	e := &Enclave{
		host:      "example.invalid",
		modelName: "test-model",
		cb:        cb,
	}
	if e.claimProbe() == nil {
		t.Fatal("expected probe claim after cooldown")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := postToEnclave(ctx, http.DefaultClient, e, "/x", nil, nil, nil); err == nil {
		t.Fatal("expected canceled request to fail")
	}
	if got := cb.State(); got != cbHalfOpen {
		t.Fatalf("breaker state = %d, want half-open (probe still owned)", got)
	}
}

type strictCloser struct {
	closes int
}

func (c *strictCloser) Read(p []byte) (int, error) { return 0, nil }
func (c *strictCloser) Close() error {
	c.closes++
	if c.closes > 1 {
		return fmt.Errorf("closed %d times", c.closes)
	}
	return nil
}

// TestInflightBodyCloseIdempotent pins that the wrapper owns the body's
// lifecycle: exactly one underlying close, exactly one gauge decrement, and
// nil on every later call.
func TestInflightBodyCloseIdempotent(t *testing.T) {
	e := &Enclave{}
	e.inflight.Add(1)
	rc := &strictCloser{}
	b := &inflightBody{ReadCloser: rc, enclave: e}

	if err := b.Close(); err != nil {
		t.Fatalf("first close = %v, want nil", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second close = %v, want nil", err)
	}
	if rc.closes != 1 {
		t.Fatalf("underlying closes = %d, want 1", rc.closes)
	}
	if got := e.inflight.Load(); got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
}
