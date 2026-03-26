package codeinterpreter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/openaiapi"
)

func TestPrepareChatInjectsToolAndPreservesToolChoice(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	req := mustParseRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"code_interpreter_options": map[string]any{},
		"tool_choice": map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": ToolName,
			},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "weather",
				},
			},
		},
	})

	prepared, err := tool.Prepare(req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared == nil {
		t.Fatal("expected active builtin")
	}

	var body map[string]any
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if _, exists := body["code_interpreter_options"]; exists {
		t.Fatal("expected code_interpreter_options to be stripped")
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected injected tool, got %d tools", len(tools))
	}
	if _, exists := body["tool_choice"]; !exists {
		t.Fatal("expected tool_choice to be preserved")
	}
}

func TestPrepareChatIgnoresNullCodeInterpreterOptions(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	req := mustParseRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"code_interpreter_options": nil,
	})

	prepared, err := tool.Prepare(req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared != nil {
		t.Fatal("expected null code_interpreter_options to leave the builtin inactive")
	}
}

func TestPrepareChatDoesNotRequireRuntimeConfiguration(t *testing.T) {
	t.Parallel()

	tool, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := mustParseRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"code_interpreter_options": map[string]any{},
	})

	prepared, err := tool.Prepare(req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared == nil {
		t.Fatal("expected active builtin")
	}
}

func TestPrepareResponsesExpandsBuiltinAndRewritesExplicitToolChoice(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	req := mustParseRequest(t, "/v1/responses", map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
		},
		"tool_choice": map[string]any{
			"type": "code_interpreter",
		},
		"include": []any{"code_interpreter_call.outputs"},
	})

	prepared, err := tool.Prepare(req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared == nil {
		t.Fatal("expected active builtin")
	}

	var body map[string]any
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected injected function tool only, got %d", len(tools))
	}
	injected, _ := tools[0].(map[string]any)
	if injected["type"] != "function" || injected["name"] != ToolName {
		t.Fatalf("unexpected injected tool: %#v", injected)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("expected tool_choice to be rewritten to \"auto\", got %#v", body["tool_choice"])
	}
}

func TestPrepareResponsesRejectsUnsupportedExplicitToolChoiceWithOtherTools(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	req := mustParseRequest(t, "/v1/responses", map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
			map[string]any{
				"type": "function",
				"name": "weather",
				"parameters": map[string]any{
					"type": "object",
				},
			},
		},
		"tool_choice": map[string]any{
			"type": "code_interpreter",
		},
	})

	if _, err := tool.Prepare(req); err == nil {
		t.Fatal("expected unsupported tool_choice error")
	}
}

func newTestTool(t *testing.T) *Tool {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	tool, err := New(Config{
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tool
}

func mustParseRequest(t *testing.T, path string, body map[string]any) *openaiapi.Request {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, handled, err := openaiapi.ParseRequest(path, nil, raw)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if !handled {
		t.Fatalf("expected typed request handling for %s", path)
	}
	return req
}
