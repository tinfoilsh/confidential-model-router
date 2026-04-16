package toolruntime

import "testing"

func TestReplaceResponsesWebSearchTools(t *testing.T) {
	replaced := replaceResponsesWebSearchTools([]any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "function", "name": "other"},
	}, []any{
		map[string]any{"type": "function", "name": "search"},
		map[string]any{"type": "function", "name": "fetch"},
	})

	if len(replaced) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(replaced))
	}
	if replaced[0].(map[string]any)["name"] != "search" {
		t.Fatalf("expected first replacement tool to be search, got %#v", replaced[0])
	}
	if replaced[1].(map[string]any)["name"] != "fetch" {
		t.Fatalf("expected second replacement tool to be fetch, got %#v", replaced[1])
	}
}

func TestReplaceResponsesWebSearchToolsDedupes(t *testing.T) {
	replaced := replaceResponsesWebSearchTools([]any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "function", "name": "other"},
		map[string]any{"type": "web_search"},
	}, []any{
		map[string]any{"type": "function", "name": "search"},
		map[string]any{"type": "function", "name": "fetch"},
	})

	counts := map[string]int{}
	for _, tool := range replaced {
		m, _ := tool.(map[string]any)
		if name, ok := m["name"].(string); ok {
			counts[name]++
		}
	}
	if counts["search"] != 1 {
		t.Fatalf("expected search injected exactly once, got %d", counts["search"])
	}
	if counts["fetch"] != 1 {
		t.Fatalf("expected fetch injected exactly once, got %d", counts["fetch"])
	}
	if counts["other"] != 1 {
		t.Fatalf("expected other tool preserved exactly once, got %d", counts["other"])
	}
	if len(replaced) != 3 {
		t.Fatalf("expected 3 tools (2 replacements + 1 pre-existing), got %d", len(replaced))
	}
}

func TestParseResponsesToolCalls(t *testing.T) {
	calls := parseResponsesToolCalls(map[string]any{
		"output": []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "search",
				"arguments": `{"query":"golang"}`,
			},
		},
	})

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].id != "call_1" || calls[0].name != "search" {
		t.Fatalf("unexpected call: %#v", calls[0])
	}
	if calls[0].arguments["query"] != "golang" {
		t.Fatalf("unexpected arguments: %#v", calls[0].arguments)
	}
}

func TestSplitToolCalls(t *testing.T) {
	routerCalls, clientCalls := splitToolCalls(map[string]struct{}{
		"search": {},
		"fetch":  {},
	}, []toolCall{
		{id: "call_1", name: "search"},
		{id: "call_2", name: "client_lookup"},
		{id: "call_3", name: "fetch"},
	})

	if len(routerCalls) != 2 {
		t.Fatalf("expected 2 router calls, got %d", len(routerCalls))
	}
	if len(clientCalls) != 1 {
		t.Fatalf("expected 1 client call, got %d", len(clientCalls))
	}
	if clientCalls[0].name != "client_lookup" {
		t.Fatalf("unexpected client call: %#v", clientCalls[0])
	}
}

func TestFilterChatToolCalls(t *testing.T) {
	message := map[string]any{
		"role": "assistant",
		"tool_calls": []any{
			map[string]any{"id": "call_1", "function": map[string]any{"name": "search"}},
			map[string]any{"id": "call_2", "function": map[string]any{"name": "client_lookup"}},
		},
	}

	filtered := filterChatToolCalls(message, []toolCall{{id: "call_1", name: "search"}})
	if filtered == nil {
		t.Fatal("expected filtered message")
	}

	rawCalls, _ := filtered["tool_calls"].([]any)
	if len(rawCalls) != 1 {
		t.Fatalf("expected 1 filtered tool call, got %d", len(rawCalls))
	}
	callMap, _ := rawCalls[0].(map[string]any)
	if callMap["id"] != "call_1" {
		t.Fatalf("unexpected filtered call: %#v", callMap)
	}
}
