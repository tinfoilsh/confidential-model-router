package codeinterpreter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientExecuteTimeout(t *testing.T) {
	t.Parallel()

	// released is closed by cleanup to unblock the hanging handler before server.Close().
	released := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/execute" {
			// Block until the client disconnects or the test cleans up.
			select {
			case <-r.Context().Done():
			case <-released:
			}
			return
		}
		// /contexts: return a valid container id.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"test-container-id"}`))
	}))
	t.Cleanup(func() {
		close(released)
		server.Close()
	})

	client := &Client{
		baseURL:     server.URL,
		httpClient:  server.Client(),
		execTimeout: 100 * time.Millisecond,
	}

	session := &Session{}
	result, err := client.Execute(context.Background(), "call-1", `{"code": "import time; time.sleep(10)"}`, session, "")
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("expected status %q on timeout, got %q", StatusFailed, result.Status)
	}
}

func TestClientNewRejectsHTTPWithRepo(t *testing.T) {
	t.Parallel()

	_, err := NewClient("http://sandbox.example.com", "owner/repo", 0)
	if err == nil {
		t.Fatal("expected error for http:// base URL with attestation repo")
	}
}

func TestClientNewAcceptsHTTPSWithRepo(t *testing.T) {
	t.Parallel()

	// We cannot actually attest in unit tests, so we only test that the URL
	// validation passes and the attestation error is about the enclave, not the URL.
	_, err := NewClient("https://sandbox.example.com", "owner/repo", 0)
	if err == nil {
		// Attestation succeeded (unlikely in unit test) — that's fine too.
		return
	}
	// The error should be about attestation, not URL scheme validation.
	for _, bad := range []string{"must use HTTPS", "scheme"} {
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("expected attestation error, got URL validation error: %v", err)
		}
	}
}

func TestClientNewAcceptsHTTPWithoutRepo(t *testing.T) {
	t.Parallel()

	// No attestation repo → HTTP is allowed (dev / local mode).
	_, err := NewClient("http://localhost:8080", "", 0)
	if err != nil {
		t.Fatalf("expected no error for http:// base URL without repo, got: %v", err)
	}
}
