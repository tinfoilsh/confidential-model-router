package toolexec

import "testing"

func TestNormalizeChatRequestInjectsToolAndPreservesToolChoice(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeChatRequest(map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"code_interpreter_options": map[string]any{},
		"tool_choice": map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": codeInterpreterToolName,
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
	if err != nil {
		t.Fatalf("normalizeChatRequest: %v", err)
	}

	if _, exists := normalized.body["code_interpreter_options"]; exists {
		t.Fatalf("expected code_interpreter_options to be stripped")
	}
	tools := rawJSONArray(normalized.body["tools"])
	if len(tools) != 2 {
		t.Fatalf("expected injected tool, got %d tools", len(tools))
	}
	if rawJSONMap(tools[1])["type"] != "function" {
		t.Fatalf("expected injected function tool, got %#v", tools[1])
	}
	if normalized.session == nil || !normalized.session.Managed {
		t.Fatalf("expected managed session for auto config")
	}
}

func TestNormalizeChatRequestRejectsCollision(t *testing.T) {
	t.Parallel()

	_, err := normalizeChatRequest(map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"code_interpreter_options": map[string]any{},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": codeInterpreterToolName,
				},
			},
		},
	})
	if err == nil {
		t.Fatalf("expected tool collision error")
	}
}

func TestNormalizeResponsesRequestExpandsShimAndStripsObjectToolChoice(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeResponsesRequest(map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
			map[string]any{"type": "web_search"},
		},
		"tool_choice": map[string]any{
			"type": "code_interpreter",
		},
		"include": []any{"code_interpreter_call.outputs"},
	})
	if err != nil {
		t.Fatalf("normalizeResponsesRequest: %v", err)
	}

	tools := rawJSONArray(normalized.body["tools"])
	if len(tools) != 2 {
		t.Fatalf("expected web_search + injected function tool, got %d tools", len(tools))
	}
	// Responses API uses a flat tool schema: {"type":"function","name":...}
	injected := rawJSONMap(tools[1])
	if jsonString(injected["type"]) != "function" || jsonString(injected["name"]) != codeInterpreterToolName {
		t.Fatalf("expected injected code_interpreter function tool, got %#v", injected)
	}

	// Object tool_choice must be stripped (backend only accepts string values).
	if _, exists := normalized.body["tool_choice"]; exists {
		t.Fatalf("expected object tool_choice to be stripped, got %#v", normalized.body["tool_choice"])
	}
	if !normalized.includeOutputs {
		t.Fatalf("expected includeOutputs to be true")
	}
}

func TestNormalizeResponsesRequestRejectsUnsupportedNetworkPolicy(t *testing.T) {
	t.Parallel()

	_, err := normalizeResponsesRequest(map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{
				"type": "code_interpreter",
				"container": map[string]any{
					"type": "auto",
					"network_policy": map[string]any{
						"type": "allowlist",
					},
				},
			},
		},
	})
	if err == nil {
		t.Fatalf("expected network policy validation error")
	}
}

func TestNormalizeResponsesRequestRejectsFunctionNameCollisionRegardlessOfOrder(t *testing.T) {
	t.Parallel()

	_, err := normalizeResponsesRequest(map[string]any{
		"model": "gpt-test",
		"input": "hi",
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": codeInterpreterToolName,
				},
			},
			map[string]any{
				"type": "code_interpreter",
			},
		},
	})
	if err == nil {
		t.Fatalf("expected tool name collision error")
	}
}
