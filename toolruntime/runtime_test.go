package toolruntime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

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

func TestModelRequestHeaders(t *testing.T) {
	source := make(http.Header)
	source.Set("Authorization", "Bearer secret")
	source.Set(manager.UsageMetricsRequestHeader, "true")
	source.Set("X-Tinfoil-Client-Requested-Usage", "true")

	headers := modelRequestHeaders(source)
	if headers.Get("Authorization") != "Bearer secret" {
		t.Fatalf("unexpected authorization header: %#v", headers)
	}
	if headers.Get(manager.UsageMetricsRequestHeader) != "true" {
		t.Fatalf("missing usage metrics header: %#v", headers)
	}
	if headers.Get("X-Tinfoil-Client-Requested-Usage") != "true" {
		t.Fatalf("missing client requested usage header: %#v", headers)
	}
}

func TestStreamChatCompletionIncludesToolCallsAndUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Tinfoil-Client-Requested-Usage", "true")

	response := &upstreamJSONResponse{
		statusCode: http.StatusOK,
		header:     http.Header{"Tinfoil-Enclave": []string{"enc-1"}},
		body: map[string]any{
			"id":      "chatcmpl_1",
			"created": float64(123),
			"model":   "gpt-oss-120b",
			"choices": []any{
				map[string]any{
					"finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"id":   "call_1",
								"type": "function",
								"function": map[string]any{
									"name":      "client_lookup",
									"arguments": "{}",
								},
							},
						},
					},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     float64(1),
				"completion_tokens": float64(0),
				"total_tokens":      float64(1),
			},
		},
	}

	if err := streamChatCompletion(rec, req, response); err != nil {
		t.Fatalf("streamChatCompletion returned error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "\"tool_calls\"") {
		t.Fatalf("expected stream to contain tool calls, got %s", body)
	}
	if !strings.Contains(body, "\"usage\"") {
		t.Fatalf("expected stream to contain usage chunk, got %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected stream terminator, got %s", body)
	}
	if rec.Header().Get("Tinfoil-Enclave") != "enc-1" {
		t.Fatalf("expected upstream headers to be preserved, got %#v", rec.Header())
	}
}

func TestStreamResponsesIncludesOutputEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	response := &upstreamJSONResponse{
		statusCode: http.StatusOK,
		header:     make(http.Header),
		body: map[string]any{
			"id":         "resp_1",
			"created_at": float64(123),
			"model":      "gpt-oss-120b",
			"output": []any{
				map[string]any{
					"id":   "msg_1",
					"type": "message",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": "Hello",
						},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  float64(1),
				"output_tokens": float64(1),
				"total_tokens":  float64(2),
			},
		},
	}

	if err := streamResponses(rec, response); err != nil {
		t.Fatalf("streamResponses returned error: %v", err)
	}

	body := rec.Body.String()
	for _, fragment := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("expected stream to contain %q, got %s", fragment, body)
		}
	}
}
