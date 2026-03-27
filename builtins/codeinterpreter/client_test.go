package codeinterpreter

import (
	"context"
	"io"
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

	_, _, err := client.Execute(context.Background(), "ctx-test", Args{Code: "import time; time.sleep(10)"}, "")
	if err == nil {
		t.Fatal("expected error on timeout, got nil")
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

func TestClientNewRejectsEmptyRepo(t *testing.T) {
	t.Parallel()

	_, err := NewClient("https://sandbox.example.com", "", 0)
	if err == nil {
		t.Fatal("expected error when attestation repo is missing")
	}
}

func TestParseArgsAcceptsContextID(t *testing.T) {
	t.Parallel()

	args, err := ParseArgs(`{"code":"print(1)","context_id":"ctx-123"}`)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if args.ContextID != "ctx-123" {
		t.Fatalf("expected context_id to be preserved, got %q", args.ContextID)
	}
}


func TestParseArgsRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	_, err := ParseArgs(`{"code":"print(1)","extra":true}`)
	if err == nil {
		t.Fatal("expected unknown field validation error")
	}
}

func TestClientExecuteUsesExplicitContextID(t *testing.T) {
	t.Parallel()

	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/execute":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("io.ReadAll: %v", err)
			}
			requests <- string(body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"stdout","text":"ok\n"}` + "\n"))
		case "/contexts":
			t.Fatal("did not expect /contexts when context_id is provided")
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := &Client{
		baseURL:     server.URL,
		httpClient:  server.Client(),
		execTimeout: 10 * time.Second,
	}

	outputs, _, err := client.Execute(context.Background(), "ctx-123", Args{Code: "print(1)", ContextID: "ctx-123"}, "")
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	_ = outputs

	body := <-requests
	if !strings.Contains(body, `"context_id":"ctx-123"`) {
		t.Fatalf("expected execute request to include context_id, got %s", body)
	}
}
