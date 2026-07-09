package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
	e := &Enclave{host: strings.TrimPrefix(ts.URL, "https://")}

	resp, err := postToEnclave(context.Background(), ts.Client(), e, "/v1/chat/completions", []byte("{}"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.inflight.Load(); got != 1 {
		t.Fatalf("in-flight after headers = %d, want 1 (body still open)", got)
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
	e2 := &Enclave{host: strings.TrimPrefix(ts2.URL, "https://")}
	if _, err := postToEnclave(context.Background(), client, e2, "/x", nil, nil); err == nil {
		t.Fatal("expected transport error against closed server")
	}
	if got := e2.inflight.Load(); got != 0 {
		t.Fatalf("in-flight after error = %d, want 0", got)
	}
}
