package codeinterpreter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientExecuteCreatesManagedContextAndCollectsOutputs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/contexts":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"ctx_123"}`)
		case "/execute":
			_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"text\":\"4\\n\"}\n")
			_, _ = io.WriteString(w, "{\"type\":\"result\",\"png\":\"YWJj\"}\n")
			_, _ = io.WriteString(w, "{\"type\":\"end_of_execution\"}\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := NewSession(ContainerConfig{Auto: &AutoConfig{}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	result, err := client.Execute(context.Background(), "call_123", `{"code":"print(2 + 2)"}`, session)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %q", result.Status)
	}
	if result.ContainerID != "ctx_123" {
		t.Fatalf("expected ctx_123 container, got %q", result.ContainerID)
	}
	if len(result.Outputs) != 2 {
		t.Fatalf("expected logs + image outputs, got %d", len(result.Outputs))
	}
	if result.Outputs[0].Type != "logs" || result.Outputs[0].Logs != "4" {
		t.Fatalf("unexpected logs output: %#v", result.Outputs[0])
	}
	if result.Outputs[1].Type != "image" || result.Outputs[1].URL == "" {
		t.Fatalf("unexpected image output: %#v", result.Outputs[1])
	}
}
