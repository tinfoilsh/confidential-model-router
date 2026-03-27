package openaiapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

type stubTool struct {
	id      string
	params  *ToolParams
	session ToolSession
	err     error
}

func (s *stubTool) ID() string {
	return s.id
}

func (s *stubTool) GetParams(*Request, Endpoint) (*ToolParams, ToolSession, error) {
	return s.params, s.session, s.err
}

type stubSession struct {
	closeCount int
	result     *ExecutionResult
}

func (s *stubSession) Execute(context.Context, *ToolCall) (*ExecutionResult, error) {
	return s.result, nil
}

func (s *stubSession) Close(context.Context) error {
	s.closeCount++
	return nil
}

func TestActivateAllowsMultipleBuiltinsAndInjectsChatToolsInOrder(t *testing.T) {
	t.Parallel()

	req := mustParseTypedRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
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

	firstSession := &stubSession{}
	secondSession := &stubSession{}
	runner := NewRunner(nil,
		&stubTool{
			id: "first",
			params: &ToolParams{
				ChatTools: []ChatToolSpec{
					mustRawJSON(t, map[string]any{"type": "function", "function": map[string]any{"name": "first_builtin"}}),
				},
				CallNames: []string{"first_builtin"},
			},
			session: firstSession,
		},
		&stubTool{
			id: "second",
			params: &ToolParams{
				ChatTools: []ChatToolSpec{
					mustRawJSON(t, map[string]any{"type": "function", "function": map[string]any{"name": "second_builtin"}}),
				},
				CallNames: []string{"second_builtin"},
			},
			session: secondSession,
		},
	)

	toolSet, err := runner.Activate(req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if toolSet == nil {
		t.Fatal("expected active tool set")
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
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}

	names := []string{
		jsonString(rawJSONMap(tools[0])["function"].(map[string]any)["name"]),
		jsonString(rawJSONMap(tools[1])["function"].(map[string]any)["name"]),
		jsonString(rawJSONMap(tools[2])["function"].(map[string]any)["name"]),
	}
	expected := []string{"weather", "first_builtin", "second_builtin"}
	for i := range expected {
		if names[i] != expected[i] {
			t.Fatalf("expected tool order %v, got %v", expected, names)
		}
	}

	if err := toolSet.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if firstSession.closeCount != 1 || secondSession.closeCount != 1 {
		t.Fatalf("expected sessions to close once, got %d and %d", firstSession.closeCount, secondSession.closeCount)
	}
}

func TestActivateRejectsBuiltInCallNameCollision(t *testing.T) {
	t.Parallel()

	req := mustParseTypedRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})

	runner := NewRunner(nil,
		&stubTool{id: "first", params: &ToolParams{CallNames: []string{"shared"}}, session: &stubSession{}},
		&stubTool{id: "second", params: &ToolParams{CallNames: []string{"shared"}}, session: &stubSession{}},
	)

	if _, err := runner.Activate(req); err == nil {
		t.Fatal("expected built-in collision error")
	}
}

func TestActivateRejectsUserToolCollision(t *testing.T) {
	t.Parallel()

	req := mustParseTypedRequest(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "code_interpreter",
				},
			},
		},
		"code_interpreter_options": map[string]any{},
	})

	runner := NewRunner(nil,
		&stubTool{id: "ci", params: &ToolParams{CallNames: []string{"code_interpreter"}}, session: &stubSession{}},
	)

	if _, err := runner.Activate(req); err == nil {
		t.Fatal("expected user collision error")
	}
}

func TestBuildResponsesRequestInterceptsBuiltinToolsAndNormalizesToolChoice(t *testing.T) {
	t.Parallel()

	req := mustParseTypedRequest(t, "/v1/responses", map[string]any{
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
			"type": "function",
			"name": "weather",
		},
	})

	runner := NewRunner(nil,
		&stubTool{
			id: "ci",
			params: &ToolParams{
				ResponseTools:          []ResponseToolSpec{mustRawJSON(t, map[string]any{"type": "function", "name": "code_interpreter"})},
				CallNames:              []string{"code_interpreter"},
				ResponseInterceptTypes: []string{"code_interpreter"},
			},
			session: &stubSession{},
		},
	)

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
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools after interception, got %d", len(tools))
	}
	if jsonString(rawJSONMap(tools[0])["name"]) != "weather" {
		t.Fatalf("expected weather tool to remain first, got %#v", tools[0])
	}
	if jsonString(rawJSONMap(tools[1])["name"]) != "code_interpreter" {
		t.Fatalf("expected injected built-in tool, got %#v", tools[1])
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("expected tool_choice to be rewritten to auto, got %#v", payload["tool_choice"])
	}
}

func TestProcessResponsesOutputExecutesBuiltinsAndPreservesUserCalls(t *testing.T) {
	t.Parallel()

	toolSet := newToolSet()
	if err := toolSet.add("ci", &ToolParams{CallNames: []string{"code_interpreter"}}, &stubSession{
		result: &ExecutionResult{
			ResponsesPublicItem: map[string]any{"type": "code_interpreter_call", "id": "ci_1"},
			ResponsesReplayItem: map[string]any{"type": "function_call_output", "call_id": "call_ci"},
		},
	}); err != nil {
		t.Fatalf("toolSet.add: %v", err)
	}

	output := []any{
		map[string]any{
			"type":      "function_call",
			"name":      "code_interpreter",
			"call_id":   "call_ci",
			"arguments": `{"code":"print(1)"}`,
		},
		map[string]any{
			"type":      "function_call",
			"name":      "weather",
			"call_id":   "call_weather",
			"arguments": `{"city":"SF"}`,
		},
	}

	processed, err := processResponsesOutput(context.Background(), output, toolSet)
	if err != nil {
		t.Fatalf("processResponsesOutput: %v", err)
	}
	if !processed.executedAny {
		t.Fatal("expected built-in execution")
	}
	if !processed.mixedWithUserTools {
		t.Fatal("expected mixed user tool flag")
	}
	if len(processed.publicItems) != 2 {
		t.Fatalf("expected 2 public items, got %d", len(processed.publicItems))
	}
	if jsonString(rawJSONMap(processed.publicItems[0])["type"]) != "code_interpreter_call" {
		t.Fatalf("expected built-in public item first, got %#v", processed.publicItems[0])
	}
	if jsonString(rawJSONMap(processed.publicItems[1])["name"]) != "weather" {
		t.Fatalf("expected unresolved user call to remain, got %#v", processed.publicItems[1])
	}
}

func mustParseTypedRequest(t *testing.T, path string, body map[string]any) *Request {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, handled, err := ParseRequest(path, http.Header{}, raw)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if !handled {
		t.Fatalf("expected typed request handling for %s", path)
	}
	return req
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return raw
}
