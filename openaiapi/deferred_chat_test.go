package openaiapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareDeferredChatFieldsForWebSearchEmbedsMarkerAndStripsCodeInterpreterOptions(t *testing.T) {
	t.Parallel()

	req := mustParseChatRequest(t, map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"web_search_options":       map[string]any{},
		"code_interpreter_options": map[string]any{"type": "auto"},
		"tool_choice":              "auto",
		"parallel_tool_calls":      true,
		"tools":                    []any{map[string]any{"type": "function", "function": map[string]any{"name": "weather"}}},
	})

	if err := req.PrepareDeferredChatFieldsForWebSearch(); err != nil {
		t.Fatalf("PrepareDeferredChatFieldsForWebSearch: %v", err)
	}
	if hasNonNullField(req.RawFields, "code_interpreter_options") {
		t.Fatal("expected top-level code_interpreter_options to be stripped before websearch hop")
	}
	if req.Chat == nil || len(req.Chat.CodeInterpreterOptions) != 0 {
		t.Fatalf("expected chat code interpreter options to be cleared, got %#v", req.Chat)
	}

	var body map[string]any
	bodyBytes, err := req.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("json.Unmarshal body: %v", err)
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("expected 2 messages with marker prepended, got %#v", body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "assistant" {
		t.Fatalf("expected marker assistant message, got %#v", first)
	}
	content, _ := first["content"].(string)
	if !strings.HasPrefix(content, deferredChatMarkerPrefix) {
		t.Fatalf("expected deferred marker content, got %#v", first["content"])
	}
}

func TestParseRequestRestoresDeferredChatFieldsAndRemovesMarker(t *testing.T) {
	t.Parallel()

	embedded := mustParseChatRequest(t, map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"web_search_options":       map[string]any{},
		"code_interpreter_options": map[string]any{"type": "auto", "ttl": 60},
		"tool_choice": map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "code_interpreter",
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
	if err := embedded.PrepareDeferredChatFieldsForWebSearch(); err != nil {
		t.Fatalf("PrepareDeferredChatFieldsForWebSearch: %v", err)
	}

	bodyBytes, err := embedded.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	var websearchBody map[string]any
	if err := json.Unmarshal(bodyBytes, &websearchBody); err != nil {
		t.Fatalf("json.Unmarshal embedded body: %v", err)
	}
	delete(websearchBody, "web_search_options")

	secondHopBody, err := json.Marshal(map[string]any{
		"model":    "gpt-test",
		"messages": websearchBody["messages"],
	})
	if err != nil {
		t.Fatalf("json.Marshal secondHopBody: %v", err)
	}

	req, handled, err := ParseRequest("/v1/chat/completions", nil, secondHopBody)
	if err != nil {
		t.Fatalf("ParseRequest second hop: %v", err)
	}
	if !handled {
		t.Fatal("expected typed request handling")
	}
	if !hasNonNullField(req.RawFields, "code_interpreter_options") {
		t.Fatal("expected restored code_interpreter_options")
	}
	if !hasNonNullField(req.RawFields, "tools") {
		t.Fatal("expected restored tools")
	}
	if !hasNonNullField(req.RawFields, "tool_choice") {
		t.Fatal("expected restored tool_choice")
	}

	var body map[string]any
	if err := json.Unmarshal(req.RawBody, &body); err != nil {
		t.Fatalf("json.Unmarshal restored body: %v", err)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("expected marker message to be removed, got %#v", body["messages"])
	}
}

func mustParseChatRequest(t *testing.T, body map[string]any) *Request {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, handled, err := ParseRequest("/v1/chat/completions", nil, raw)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if !handled {
		t.Fatal("expected typed request handling")
	}
	return req
}
