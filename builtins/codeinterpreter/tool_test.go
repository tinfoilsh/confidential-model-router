package codeinterpreter

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/openaiapi"
)

func TestChatActivationInjectsToolAndPreservesToolChoice(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	runner := openaiapi.NewRunner(nil, tool)
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

	toolSet, err := runner.Activate(req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if toolSet == nil {
		t.Fatal("expected active builtin")
	}

	body, err := runner.BuildRequestBody(req, toolSet)
	if err != nil {
		t.Fatalf("BuildRequestBody: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if _, exists := payload["code_interpreter_options"]; exists {
		t.Fatal("expected code_interpreter_options to be stripped")
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected injected tool, got %d tools", len(tools))
	}
	if _, exists := payload["tool_choice"]; !exists {
		t.Fatal("expected tool_choice to be preserved")
	}
}

func TestChatActivationIgnoresNullCodeInterpreterOptions(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	req := mustParseRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"code_interpreter_options": nil,
	})

	params, session, err := tool.GetParams(req, openaiapi.EndpointChatCompletions)
	if err != nil {
		t.Fatalf("GetParams: %v", err)
	}
	if params != nil || session != nil {
		t.Fatal("expected null code_interpreter_options to leave the builtin inactive")
	}
}

func TestChatActivationDoesNotRequireRuntimeConfiguration(t *testing.T) {
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

	params, session, err := tool.GetParams(req, openaiapi.EndpointChatCompletions)
	if err != nil {
		t.Fatalf("GetParams: %v", err)
	}
	if params == nil || session == nil {
		t.Fatal("expected active builtin")
	}
}

func TestResponsesActivationExpandsBuiltinAndRewritesExplicitToolChoice(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	runner := openaiapi.NewRunner(nil, tool)
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

	toolSet, err := runner.Activate(req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	body, err := runner.BuildRequestBody(req, toolSet)
	if err != nil {
		t.Fatalf("BuildRequestBody: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected injected function tool only, got %d", len(tools))
	}
	injected, _ := tools[0].(map[string]any)
	if injected["type"] != "function" || injected["name"] != ToolName {
		t.Fatalf("unexpected injected tool: %#v", injected)
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("expected tool_choice to be rewritten to auto, got %#v", payload["tool_choice"])
	}
}

func TestResponsesActivationRewritesRequiredToolChoice(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	runner := openaiapi.NewRunner(nil, tool)
	req := mustParseRequest(t, "/v1/responses", map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
		},
		"tool_choice": "required",
	})

	toolSet, err := runner.Activate(req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	body, err := runner.BuildRequestBody(req, toolSet)
	if err != nil {
		t.Fatalf("BuildRequestBody: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("expected required tool_choice to be rewritten to auto, got %#v", payload["tool_choice"])
	}
}

func TestResponsesActivationRejectsUnsupportedExplicitToolChoiceWithOtherTools(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	runner := openaiapi.NewRunner(nil, tool)
	req := mustParseRequest(t, "/v1/responses", map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
			map[string]any{
				"type":       "function",
				"name":       "weather",
				"parameters": map[string]any{"type": "object"},
			},
		},
		"tool_choice": map[string]any{
			"type": "code_interpreter",
		},
	})

	toolSet, err := runner.Activate(req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if _, err := runner.BuildRequestBody(req, toolSet); err == nil {
		t.Fatal("expected unsupported tool_choice error")
	}
}

func TestResponsesActivationReturnsLazySession(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)
	req := mustParseRequestWithHeader(t, "/v1/responses", http.Header{
		"Authorization": []string{"Bearer sk-user"},
	}, map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
		},
		"include": []any{"code_interpreter_call.outputs"},
	})

	params, session, err := tool.GetParams(req, openaiapi.EndpointResponses)
	if err != nil {
		t.Fatalf("GetParams: %v", err)
	}
	if params == nil || session == nil {
		t.Fatal("expected active builtin")
	}

	requestSession, ok := session.(*toolSession)
	if !ok || requestSession == nil {
		t.Fatalf("expected request-scoped tool session, got %T", session)
	}
	if !requestSession.includeOutputs {
		t.Fatal("expected session to preserve includeOutputs")
	}
	if requestSession.runtime != nil {
		t.Fatal("expected runtime to remain lazy until execute")
	}
}

func newTestTool(t *testing.T) *Tool {
	t.Helper()
	tool, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tool
}

func mustParseRequest(t *testing.T, path string, body map[string]any) *openaiapi.Request {
	t.Helper()
	return mustParseRequestWithHeader(t, path, nil, body)
}

func mustParseRequestWithHeader(t *testing.T, path string, header http.Header, body map[string]any) *openaiapi.Request {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, handled, err := openaiapi.ParseRequest(path, header, raw)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if !handled {
		t.Fatalf("expected typed request handling for %s", path)
	}
	return req
}
