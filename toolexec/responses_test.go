package toolexec

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/toolexec/codeinterpreter"
)

func TestProcessResponsesOutputRewritesCodeInterpreterCalls(t *testing.T) {
	t.Parallel()

	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/contexts":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"ctx_123"}`)
		case "/execute":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"text\":\"4\\n\"}\n{\"type\":\"end_of_execution\"}\n")
		default:
			http.NotFound(w, r)
		}
	})

	session, err := executor.newManagedSession()
	if err != nil {
		t.Fatalf("newManagedSession: %v", err)
	}

	processed, err := executor.processResponsesOutput(context.Background(), []any{
		map[string]any{
			"id":        "fc_item_123",
			"type":      "function_call",
			"call_id":   "call_123",
			"name":      codeInterpreterToolName,
			"arguments": `{"code":"print(2 + 2)"}`,
		},
	}, session, true)
	if err != nil {
		t.Fatalf("processResponsesOutput: %v", err)
	}

	if !processed.executedAny {
		t.Fatalf("expected code interpreter execution")
	}
	if processed.mixedWithUserTools {
		t.Fatalf("did not expect mixed tool flag")
	}
	if len(processed.publicItems) != 1 {
		t.Fatalf("expected 1 public item, got %d", len(processed.publicItems))
	}
	publicItem := rawJSONMap(processed.publicItems[0])
	if jsonString(publicItem["type"]) != "code_interpreter_call" {
		t.Fatalf("expected code_interpreter_call, got %#v", publicItem)
	}
	if jsonString(publicItem["container_id"]) != "ctx_123" {
		t.Fatalf("expected container_id ctx_123, got %#v", publicItem)
	}
	outputs := rawJSONArray(publicItem["outputs"])
	if len(outputs) != 1 || jsonString(rawJSONMap(outputs[0])["logs"]) != "4" {
		t.Fatalf("unexpected outputs: %#v", outputs)
	}
	if len(processed.functionCallOutputs) != 1 {
		t.Fatalf("expected function_call_output replay, got %d", len(processed.functionCallOutputs))
	}
}

func TestProcessResponsesOutputFlagsMixedUserTools(t *testing.T) {
	t.Parallel()

	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/contexts":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"ctx_123"}`)
		case "/execute":
			_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"text\":\"ok\\n\"}\n{\"type\":\"end_of_execution\"}\n")
		default:
			http.NotFound(w, r)
		}
	})

	session, err := executor.newManagedSession()
	if err != nil {
		t.Fatalf("newManagedSession: %v", err)
	}

	processed, err := executor.processResponsesOutput(context.Background(), []any{
		map[string]any{
			"id":        "fc_item_123",
			"type":      "function_call",
			"call_id":   "call_123",
			"name":      codeInterpreterToolName,
			"arguments": `{"code":"print('ok')"}`,
		},
		map[string]any{
			"id":        "fc_item_456",
			"type":      "function_call",
			"call_id":   "call_456",
			"name":      "weather",
			"arguments": `{"city":"Paris"}`,
		},
	}, session, false)
	if err != nil {
		t.Fatalf("processResponsesOutput: %v", err)
	}

	if !processed.executedAny {
		t.Fatalf("expected code interpreter execution")
	}
	if !processed.mixedWithUserTools {
		t.Fatalf("expected mixed user tool flag")
	}
	if len(processed.publicItems) != 2 {
		t.Fatalf("expected both public items, got %d", len(processed.publicItems))
	}
}

type testExecutor struct {
	*Executor
}

func newTestExecutor(t *testing.T, handler http.HandlerFunc) *testExecutor {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	executor, err := New(server.URL, "", time.Second)
	if err != nil {
		t.Fatalf("New executor: %v", err)
	}
	return &testExecutor{Executor: executor}
}

func (e *testExecutor) newManagedSession() (*codeinterpreter.Session, error) {
	return codeinterpreter.NewSession(codeinterpreter.ContainerConfig{
		Auto: &codeinterpreter.AutoConfig{},
	})
}
