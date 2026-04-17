package toolruntime

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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

// TestForcedFinalChatRequestStripsToolSelection pins that tool_choice
// and function_call are removed alongside tools, so the fallback
// request cannot carry a protocol-level instruction to call a tool
// that no longer exists on the request.
func TestForcedFinalChatRequestStripsToolSelection(t *testing.T) {
	reqBody := map[string]any{
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":         []any{map[string]any{"type": "function"}},
		"tool_choice":   "required",
		"function_call": "auto",
	}
	finalBody := forcedFinalChatRequest(reqBody)
	for _, field := range []string{"tools", "tool_choice", "function_call"} {
		if _, ok := finalBody[field]; ok {
			t.Fatalf("expected %s to be stripped, got %#v", field, finalBody[field])
		}
	}
}

// TestForcedFinalResponsesRequestStripsToolSelection is the Responses
// API counterpart; tool_choice must be removed alongside tools.
func TestForcedFinalResponsesRequestStripsToolSelection(t *testing.T) {
	reqBody := map[string]any{
		"input":       []map[string]any{{"type": "message", "role": "user", "content": "hi"}},
		"tools":       []any{map[string]any{"type": "function"}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "search"}},
	}
	finalBody := forcedFinalResponsesRequest(reqBody)
	for _, field := range []string{"tools", "tool_choice"} {
		if _, ok := finalBody[field]; ok {
			t.Fatalf("expected %s to be stripped, got %#v", field, finalBody[field])
		}
	}
}

// TestBuildWebSearchCallOutputItemsMarksFailedCalls pins that a tool
// record with a non-empty errorReason surfaces as a web_search_call
// output item with status:"failed" (not "completed"), so the terminal
// Responses snapshot is honest about what actually happened.
func TestBuildWebSearchCallOutputItemsMarksFailedCalls(t *testing.T) {
	records := []toolCallRecord{
		{
			name:        "search",
			arguments:   map[string]any{"query": "latest news"},
			errorReason: "upstream timeout",
		},
		{
			name:      "search",
			arguments: map[string]any{"query": "ok"},
		},
	}
	items := buildWebSearchCallOutputItems(records)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	failed, _ := items[0].(map[string]any)
	if got := stringValue(failed["status"]); got != "failed" {
		t.Fatalf("expected failed status on errored call, got %q", got)
	}
	completed, _ := items[1].(map[string]any)
	if got := stringValue(completed["status"]); got != "completed" {
		t.Fatalf("expected completed status on successful call, got %q", got)
	}
}

// TestPublicToolErrorReasonStripsInternalDetail pins that the helper
// used at every client-facing error boundary returns a short constant
// and NEVER the raw error text. This is the invariant that prevents
// internal hostnames, gRPC error bodies, context-cancellation traces,
// or other implementation details from leaking into web_search_call
// `reason` fields that clients render verbatim.
func TestPublicToolErrorReasonStripsInternalDetail(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	cases := []error{
		errors.New("dial tcp 10.0.0.5:8443: connect: connection refused"),
		errors.New("rpc error: code = Unavailable desc = envoy upstream connect error"),
		errors.New("context deadline exceeded"),
		fmt.Errorf("internal stack trace: %s", "goroutine 42 [select]:"),
	}
	for _, err := range cases {
		got := publicToolErrorReason("search", err)
		if got != publicToolErrorReasonString {
			t.Fatalf("publicToolErrorReason(%q) = %q, want constant %q", err, got, publicToolErrorReasonString)
		}
		if strings.Contains(got, "tcp") || strings.Contains(got, "envoy") || strings.Contains(got, "goroutine") {
			t.Fatalf("public reason leaked internal detail: %q", got)
		}
	}
	if got := publicToolErrorReason("search", nil); got != "" {
		t.Fatalf("nil err should return empty string, got %q", got)
	}
}
