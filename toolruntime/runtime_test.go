package toolruntime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
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
	source.Set("OpenAI-Organization", "org_123")
	source.Set(manager.UsageMetricsRequestHeader, "true")
	source.Set("X-Tinfoil-Client-Requested-Usage", "true")

	headers := modelRequestHeaders(source)
	if headers.Get("Authorization") != "Bearer secret" {
		t.Fatalf("unexpected authorization header: %#v", headers)
	}
	if headers.Get("OpenAI-Organization") != "org_123" {
		t.Fatalf("expected passthrough header, got %#v", headers)
	}
	if headers.Get(manager.UsageMetricsRequestHeader) != "" {
		t.Fatalf("expected usage metrics header to be stripped, got %#v", headers)
	}
	if headers.Get("X-Tinfoil-Client-Requested-Usage") != "" {
		t.Fatalf("expected client usage header to be stripped, got %#v", headers)
	}
	if headers.Get("X-Tinfoil-Internal-Tool-Loop") != "" {
		t.Fatalf("expected no internal tool loop header on forwarded request, got %#v", headers)
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

	if err := streamChatCompletion(rec, req, response, true); err != nil {
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
	if rec.Result().Trailer.Get(manager.UsageMetricsResponseHeader) != "prompt=1,completion=0,total=1" {
		t.Fatalf("expected usage trailer, got %#v", rec.Result().Trailer)
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

	if err := streamResponses(rec, response, true); err != nil {
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
	if rec.Result().Trailer.Get(manager.UsageMetricsResponseHeader) != "prompt=1,completion=1,total=2" {
		t.Fatalf("expected usage trailer, got %#v", rec.Result().Trailer)
	}
}

func TestApplyAggregatedUsageReplacesResponsesTotals(t *testing.T) {
	response := &upstreamJSONResponse{
		body: map[string]any{
			"usage": map[string]any{
				"input_tokens":  float64(2),
				"output_tokens": float64(3),
				"total_tokens":  float64(5),
			},
		},
		header: make(http.Header),
	}

	applyAggregatedUsage(response, "/v1/responses", &tokencount.Usage{
		PromptTokens:     7,
		CompletionTokens: 11,
		TotalTokens:      18,
	})

	usage := usageFromRaw(response.body["usage"])
	if usage == nil {
		t.Fatal("expected aggregated usage")
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 11 || usage.TotalTokens != 18 {
		t.Fatalf("unexpected aggregated usage: %#v", usage)
	}
}

func TestFinalizeToolLoopResponseAggregatesForcedFinalChatUsage(t *testing.T) {
	response := &upstreamJSONResponse{
		body: map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": "done",
					},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     float64(2),
				"completion_tokens": float64(3),
				"total_tokens":      float64(5),
			},
		},
		header: make(http.Header),
	}
	citations := &citationState{
		toolCalls: []toolCallRecord{
			{
				name:      "search",
				arguments: map[string]any{"query": "cats"},
			},
		},
	}

	finalizeToolLoopResponse(response, "/v1/chat/completions", &tokencount.Usage{
		PromptTokens:     7,
		CompletionTokens: 11,
		TotalTokens:      18,
	}, citations)

	usage := usageFromRaw(response.body["usage"])
	if usage == nil {
		t.Fatal("expected aggregated usage")
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 11 || usage.TotalTokens != 18 {
		t.Fatalf("unexpected aggregated usage: %#v", usage)
	}

	extras, _ := response.body[routerChatExtrasKey].(*routerChatExtras)
	if extras == nil || len(extras.toolCalls) != 1 {
		t.Fatalf("expected chat citation extras to be attached, got %#v", response.body[routerChatExtrasKey])
	}
}

func TestFinalizeToolLoopResponseAggregatesForcedFinalResponsesUsage(t *testing.T) {
	response := &upstreamJSONResponse{
		body: map[string]any{
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": "done",
						},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  float64(2),
				"output_tokens": float64(3),
				"total_tokens":  float64(5),
			},
		},
		header: make(http.Header),
	}
	citations := &citationState{
		toolCalls: []toolCallRecord{
			{
				name:       "search",
				arguments:  map[string]any{"query": "cats"},
				resultURLs: []string{"https://example.com"},
			},
		},
	}

	finalizeToolLoopResponse(response, "/v1/responses", &tokencount.Usage{
		PromptTokens:     7,
		CompletionTokens: 11,
		TotalTokens:      18,
	}, citations)

	usage := usageFromRaw(response.body["usage"])
	if usage == nil {
		t.Fatal("expected aggregated usage")
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 11 || usage.TotalTokens != 18 {
		t.Fatalf("unexpected aggregated usage: %#v", usage)
	}

	output, _ := response.body["output"].([]any)
	if len(output) == 0 {
		t.Fatal("expected response output")
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "web_search_call" {
		t.Fatalf("expected web search call output to be prepended, got %#v", output[0])
	}
}

func TestNormalizeResponsesInputWrapsStringPrompt(t *testing.T) {
	items := normalizeResponsesInput("hello world")
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	item, _ := items[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("unexpected message wrapper: %#v", item)
	}

	content, _ := item["content"].([]map[string]any)
	if len(content) != 1 || content[0]["text"] != "hello world" {
		t.Fatalf("unexpected content: %#v", item["content"])
	}
}

func TestPrependResponsesPromptPreservesPromptItems(t *testing.T) {
	prompt := &mcp.GetPromptResult{
		Messages: []*mcp.PromptMessage{
			{
				Role:    "system",
				Content: &mcp.TextContent{Text: "system prompt"},
			},
		},
	}

	items := prependResponsesPrompt(prompt, "user question")
	list, _ := items.([]any)
	if len(list) != 3 {
		t.Fatalf("expected 3 items, got %d", len(list))
	}

	first, _ := list[0].(map[string]any)
	second, _ := list[1].(map[string]any)
	third, _ := list[2].(map[string]any)
	if fmt.Sprint(first["role"]) != "system" || fmt.Sprint(second["role"]) != "system" || fmt.Sprint(third["role"]) != "user" {
		t.Fatalf("unexpected prompt ordering: %#v", list)
	}
	if !strings.Contains(fmt.Sprint(first["content"]), "Current date and time:") {
		t.Fatalf("expected context message first, got %#v", first)
	}
}

func TestPrependChatPromptAddsCurrentDateContext(t *testing.T) {
	prompt := &mcp.GetPromptResult{
		Messages: []*mcp.PromptMessage{
			{
				Role:    "system",
				Content: &mcp.TextContent{Text: "system prompt"},
			},
		},
	}

	messages := prependChatPrompt(prompt, []any{
		map[string]any{"role": "user", "content": "user question"},
	})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	first, _ := messages[0].(map[string]any)
	second, _ := messages[1].(map[string]any)
	if fmt.Sprint(first["role"]) != "system" || !strings.Contains(fmt.Sprint(first["content"]), "Current date and time:") {
		t.Fatalf("unexpected context message: %#v", first)
	}
	if fmt.Sprint(second["role"]) != "system" || fmt.Sprint(second["content"]) != "system prompt" {
		t.Fatalf("unexpected prompt message ordering: %#v", messages)
	}
}

func TestFormatStructuredToolOutputLabelsSearchResults(t *testing.T) {
	state := &citationState{nextIndex: 1}
	formatted := formatStructuredToolOutput("search", map[string]any{
		"results": []any{
			map[string]any{
				"title":          "Result A",
				"url":            "https://example.com/a",
				"content":        "Alpha",
				"published_date": "2026-04-17",
			},
			map[string]any{
				"title":   "Result B",
				"url":     "https://example.com/b",
				"content": "Beta",
			},
		},
	}, state)

	for _, fragment := range []string{
		"Source: Result A",
		"URL: https://example.com/a",
		"Published: 2026-04-17",
		"Source: Result B",
		"URL: https://example.com/b",
	} {
		if !strings.Contains(formatted, fragment) {
			t.Fatalf("expected %q in formatted output, got %q", fragment, formatted)
		}
	}
	if strings.Contains(formatted, "【") {
		t.Fatalf("tool output should not include numbered markers, got %q", formatted)
	}
	if len(state.sources) != 2 {
		t.Fatalf("expected 2 recorded sources, got %d", len(state.sources))
	}
}

func TestFormatStructuredToolOutputLabelsFetchPages(t *testing.T) {
	state := &citationState{nextIndex: 1}
	formatted := formatStructuredToolOutput("fetch", map[string]any{
		"pages": []any{
			map[string]any{
				"url":     "https://example.com/page",
				"content": "Page body",
			},
		},
	}, state)

	for _, fragment := range []string{"Source: Fetched page", "URL: https://example.com/page", "Page body"} {
		if !strings.Contains(formatted, fragment) {
			t.Fatalf("expected %q in formatted output, got %q", fragment, formatted)
		}
	}
	if strings.Contains(formatted, "【") {
		t.Fatalf("tool output should not include numbered markers, got %q", formatted)
	}
	if len(state.sources) != 1 {
		t.Fatalf("expected 1 recorded source, got %d", len(state.sources))
	}
}

func TestNormalizeResponsesOutputItemsConvertsMCPCalls(t *testing.T) {
	items := normalizeResponsesOutputItems([]any{
		map[string]any{
			"type":      "mcp_call",
			"id":        "call_1",
			"name":      "search",
			"arguments": "{\"query\":\"cats\"}",
		},
	})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	item, _ := items[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_1" {
		t.Fatalf("unexpected normalized item: %#v", item)
	}
}

func TestForcedFinalChatRequestRemovesToolsAndAppendsInstruction(t *testing.T) {
	reqBody := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"tools": []any{
			map[string]any{"type": "function"},
		},
	}

	finalBody := forcedFinalChatRequest(reqBody)
	if _, ok := finalBody["tools"]; ok {
		t.Fatalf("expected tools to be removed, got %#v", finalBody["tools"])
	}

	messages, _ := finalBody["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	last, _ := messages[1].(map[string]any)
	if last["role"] != "system" || last["content"] != finalAnswerInstructionText {
		t.Fatalf("unexpected final instruction: %#v", last)
	}
}

func TestForcedFinalResponsesRequestRemovesToolsAndAppendsInstruction(t *testing.T) {
	reqBody := map[string]any{
		"input": []map[string]any{
			{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "done",
			},
		},
		"tools": []any{
			map[string]any{"type": "function"},
		},
	}

	finalBody := forcedFinalResponsesRequest(reqBody)
	if _, ok := finalBody["tools"]; ok {
		t.Fatalf("expected tools to be removed, got %#v", finalBody["tools"])
	}

	input, _ := finalBody["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("expected 2 input items, got %d", len(input))
	}

	first, _ := input[0].(map[string]any)
	if first["type"] != "message" || first["role"] != "system" {
		t.Fatalf("unexpected converted tool result: %#v", first)
	}

	last, _ := input[1].(map[string]any)
	if last["type"] != "message" || last["role"] != "system" || fmt.Sprint(last["content"]) == "" {
		t.Fatalf("unexpected final input item: %#v", last)
	}
}
