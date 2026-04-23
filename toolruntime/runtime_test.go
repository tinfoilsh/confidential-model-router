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
		map[string]any{"type": "function", "name": routerSearchToolName},
		map[string]any{"type": "function", "name": routerFetchToolName},
	})

	if len(replaced) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(replaced))
	}
	if replaced[0].(map[string]any)["name"] != routerSearchToolName {
		t.Fatalf("expected first replacement tool to be %s, got %#v", routerSearchToolName, replaced[0])
	}
	if replaced[1].(map[string]any)["name"] != routerFetchToolName {
		t.Fatalf("expected second replacement tool to be %s, got %#v", routerFetchToolName, replaced[1])
	}
}

func TestReplaceResponsesWebSearchToolsDedupes(t *testing.T) {
	replaced := replaceResponsesWebSearchTools([]any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "function", "name": "other"},
		map[string]any{"type": "web_search"},
	}, []any{
		map[string]any{"type": "function", "name": routerSearchToolName},
		map[string]any{"type": "function", "name": routerFetchToolName},
	})

	counts := map[string]int{}
	for _, tool := range replaced {
		m, _ := tool.(map[string]any)
		if name, ok := m["name"].(string); ok {
			counts[name]++
		}
	}
	if counts[routerSearchToolName] != 1 {
		t.Fatalf("expected %s injected exactly once, got %d", routerSearchToolName, counts[routerSearchToolName])
	}
	if counts[routerFetchToolName] != 1 {
		t.Fatalf("expected %s injected exactly once, got %d", routerFetchToolName, counts[routerFetchToolName])
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
		routerSearchToolName: {},
		routerFetchToolName:  {},
	}, []toolCall{
		{id: "call_1", name: routerSearchToolName},
		{id: "call_2", name: "client_lookup"},
		{id: "call_3", name: routerFetchToolName},
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

func TestResponsesAdapterApplyUsageReplacesResponsesTotals(t *testing.T) {
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

	adapter := newResponsesLoopAdapter(map[string]any{}, nil, nil, nil)
	adapter.applyUsage(response, &tokencount.Usage{
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

func TestChatAdapterFinalizeAggregatesForcedFinalUsage(t *testing.T) {
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

	adapter := newChatLoopAdapter(map[string]any{}, nil, nil, nil, "m", http.Header{})
	adapter.applyUsage(response, &tokencount.Usage{
		PromptTokens:     7,
		CompletionTokens: 11,
		TotalTokens:      18,
	})
	adapter.attachCitations(response.body, citations, false)

	usage := usageFromRaw(response.body["usage"])
	if usage == nil {
		t.Fatal("expected aggregated usage")
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 11 || usage.TotalTokens != 18 {
		t.Fatalf("unexpected aggregated usage: %#v", usage)
	}
}

func TestResponsesAdapterFinalizeAggregatesForcedFinalUsage(t *testing.T) {
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

	adapter := newResponsesLoopAdapter(map[string]any{
		"include": []any{includeActionSourcesFlag},
	}, nil, nil, nil)
	adapter.applyUsage(response, &tokencount.Usage{
		PromptTokens:     7,
		CompletionTokens: 11,
		TotalTokens:      18,
	})
	adapter.attachCitations(response.body, citations, false)

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
				"type":    "function_call",
				"call_id": "call_1",
				"name":    "search",
				"args":    "{}",
			},
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
	// The two prior conversation items (function_call +
	// function_call_output) must ride through untouched so the
	// call_id binding upstream validates against still holds,
	// followed by the single appended system-role instruction
	// that forces a final answer.
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}

	// Tool-call items must NOT be rewritten into messages. In
	// particular, function_call_output must stay typed so the
	// scraped web content inside it keeps the router's trust
	// boundary: tool output is data, not a system instruction.
	fc, _ := input[0].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" {
		t.Fatalf("expected function_call to pass through unchanged, got %#v", fc)
	}
	fco, _ := input[1].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" {
		t.Fatalf("expected function_call_output to pass through unchanged, got %#v", fco)
	}

	last, _ := input[2].(map[string]any)
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
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": routerSearchToolName}},
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
	items := buildWebSearchCallOutputItems(records, false)
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

// TestPublicToolErrorReasonMarksSafetyBlocked pins that safety-blocked
// errors (PII or prompt-injection filter trips) surface the dedicated
// `blocked_by_safety_filter` reason rather than collapsing into the
// generic `tool_error` constant. This is what clients key off to render
// the corresponding web_search_call status as "blocked".
func TestPublicToolErrorReasonMarksSafetyBlocked(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	err := errors.New("query was blocked by safety filters: pii detected — rephrase and retry")
	got := publicToolErrorReason("search", err)
	if got != blockedToolErrorReason {
		t.Fatalf("expected %q for safety-blocked error, got %q", blockedToolErrorReason, got)
	}
	if strings.Contains(got, "pii detected") {
		t.Fatalf("public reason leaked detail: %q", got)
	}
}

// TestFailureStatusForMapsSafetyBlock pins the web_search_call status
// returned for each error class used on the streaming error paths.
func TestFailureStatusForMapsSafetyBlock(t *testing.T) {
	if got := failureStatusFor(errors.New("Query was blocked by safety filters: injection")); got != "blocked" {
		t.Fatalf("expected blocked, got %q", got)
	}
	if got := failureStatusFor(errors.New("upstream timeout")); got != "failed" {
		t.Fatalf("expected failed, got %q", got)
	}
}

// TestBuildWebSearchCallOutputItemsCollapsesBlockedToFailed pins that a
// recorded search whose errorReason is the safety-block constant
// surfaces as status:"failed" on the spec-conformant web_search_call
// output item (OpenAI's status enum has no "blocked" value). The
// distinctive blocked signal is carried via tinfoil-event markers when
// the caller opts in.
func TestBuildWebSearchCallOutputItemsCollapsesBlockedToFailed(t *testing.T) {
	records := []toolCallRecord{
		{
			name:        "search",
			arguments:   map[string]any{"query": "sensitive"},
			errorReason: blockedToolErrorReason,
		},
	}
	items := buildWebSearchCallOutputItems(records, false)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item, _ := items[0].(map[string]any)
	if got := stringValue(item["status"]); got != "failed" {
		t.Fatalf("expected failed status (blocked collapsed for spec compat), got %q", got)
	}
	if _, extra := item["reason"]; extra {
		t.Fatalf("spec-conformant output item must not carry a reason field")
	}
}

// TestWebSearchCallEventMatchesOpenAISpec pins that every synthesized
// web_search_call output item carries exactly the fields OpenAI's
// Responses API documents: type, id, status, action. In particular it
// does NOT duplicate `item_id` onto the non-streaming output-item shape
// (item_id is specced only on the streaming envelope), and it never
// carries a non-spec `reason` field. The tinfoil-specific detail (raw
// router status + error code) rides on a `_tinfoil` sidecar that is
// omitted entirely on the happy path.
func TestWebSearchCallEventMatchesOpenAISpec(t *testing.T) {
	event := webSearchCallEvent("ws_1", "in_progress", "", map[string]any{"type": "search"})
	if got := stringValue(event["id"]); got != "ws_1" {
		t.Fatalf("expected id=ws_1, got %q", got)
	}
	if got := stringValue(event["type"]); got != "web_search_call" {
		t.Fatalf("expected type=web_search_call, got %q", got)
	}
	if _, extra := event["item_id"]; extra {
		t.Fatalf("output item must not duplicate item_id onto the non-streaming shape")
	}
	if _, extra := event["reason"]; extra {
		t.Fatalf("output item must not carry a non-spec reason field")
	}
	if _, extra := event["_tinfoil"]; extra {
		t.Fatalf("happy-path output item must not carry a _tinfoil sidecar")
	}
}

// TestWebSearchCallEventCollapsesBlockedToFailed pins that the spec-only
// web_search_call output-item status set is preserved: OpenAI documents
// in_progress / searching / completed / failed, with no `blocked`. The
// router's internal blocked status is collapsed onto failed on the
// envelope, and the unfiltered `blocked` string rides on the `_tinfoil`
// vendor-extension field alongside the opaque error code. Strict SDKs
// ignore `_tinfoil`; Tinfoil-aware clients read it directly off the
// item to distinguish safety blocks from generic failures.
func TestWebSearchCallEventCollapsesBlockedToFailed(t *testing.T) {
	event := webSearchCallEvent("ws_1", "blocked", blockedToolErrorReason, map[string]any{"type": "search"})
	if got := stringValue(event["status"]); got != "failed" {
		t.Fatalf("expected blocked to collapse onto failed on the output item, got %q", got)
	}
	sidecar, ok := event["_tinfoil"].(map[string]any)
	if !ok {
		t.Fatalf("blocked tool call must carry a _tinfoil sidecar, got %#v", event["_tinfoil"])
	}
	if got := stringValue(sidecar["status"]); got != "blocked" {
		t.Fatalf("_tinfoil.status must carry the unfiltered blocked string, got %q", got)
	}
	errObj, _ := sidecar["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("_tinfoil sidecar must include an error object for errored calls: %#v", sidecar)
	}
	if got := stringValue(errObj["code"]); got != blockedToolErrorReason {
		t.Fatalf("_tinfoil.error.code must carry the router error code, got %q", got)
	}
}

// TestWebSearchCallEventGenericFailureCarriesErrorCode pins that a
// non-blocked failure still produces a `_tinfoil.error.code` field so
// clients can branch UI on the specific failure reason, but omits the
// `_tinfoil.status` field because the envelope `status: failed` is the
// truthful carrier of that signal.
func TestWebSearchCallEventGenericFailureCarriesErrorCode(t *testing.T) {
	event := webSearchCallEvent("ws_1", "failed", "upstream_timeout", map[string]any{"type": "search"})
	if got := stringValue(event["status"]); got != "failed" {
		t.Fatalf("expected envelope status=failed, got %q", got)
	}
	sidecar, ok := event["_tinfoil"].(map[string]any)
	if !ok {
		t.Fatalf("errored tool call must carry a _tinfoil sidecar, got %#v", event["_tinfoil"])
	}
	if _, present := sidecar["status"]; present {
		t.Fatalf("_tinfoil.status must be omitted when envelope status already carries the signal: %#v", sidecar)
	}
	errObj, _ := sidecar["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("_tinfoil sidecar must include an error object: %#v", sidecar)
	}
	if got := stringValue(errObj["code"]); got != "upstream_timeout" {
		t.Fatalf("_tinfoil.error.code = %q, want upstream_timeout", got)
	}
}

// TestBuildWebSearchCallOutputItemsGatesActionSources pins the include
// opt-in: without `web_search_call.action.sources` the field is
// omitted entirely; with it, the URLs are shipped in OpenAI's
// documented `[{type:"url", url:"..."}]` shape.
func TestBuildWebSearchCallOutputItemsGatesActionSources(t *testing.T) {
	records := []toolCallRecord{
		{
			name:       "search",
			arguments:  map[string]any{"query": "cats"},
			resultURLs: []string{"https://a.test", "https://b.test"},
		},
	}

	withoutInclude := buildWebSearchCallOutputItems(records, false)
	item, _ := withoutInclude[0].(map[string]any)
	action, _ := item["action"].(map[string]any)
	if _, present := action["sources"]; present {
		t.Fatalf("expected sources omitted when include flag is false, got %#v", action)
	}

	withInclude := buildWebSearchCallOutputItems(records, true)
	item, _ = withInclude[0].(map[string]any)
	action, _ = item["action"].(map[string]any)
	sources, _ := action["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %#v", sources)
	}
	first, _ := sources[0].(map[string]any)
	if stringValue(first["type"]) != "url" {
		t.Fatalf("expected source type=url, got %#v", first)
	}
	if stringValue(first["url"]) != "https://a.test" {
		t.Fatalf("expected first source url=https://a.test, got %#v", first)
	}
}

// TestParseIncludeActionSourcesReadsRequestFlag pins the request body
// parser for OpenAI's documented `include: ["..."]` opt-in list.
func TestParseIncludeActionSourcesReadsRequestFlag(t *testing.T) {
	body := map[string]any{"include": []any{"web_search_call.action.sources"}}
	if !parseIncludeActionSources(body) {
		t.Fatal("expected flag to parse as true")
	}
	if parseIncludeActionSources(map[string]any{"include": []any{"something.else"}}) {
		t.Fatal("unrelated include entries must not trigger the flag")
	}
	if parseIncludeActionSources(map[string]any{}) {
		t.Fatal("empty body must parse as false")
	}
}

// TestToolResultErrorMessageExtractsHandlerText pins that handler
// errors packed into CallToolResult.Content (MCP's convention for tool
// errors vs protocol errors) are surfaced as a single joined string the
// router can feed into failureStatusFor / isToolCallBlocked.
func TestToolResultErrorMessageExtractsHandlerText(t *testing.T) {
	result := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: "query was blocked by safety filters: PII detected"},
		},
	}
	if got := toolResultErrorMessage(result); !strings.Contains(got, "blocked by safety filters") {
		t.Fatalf("expected blocked message, got %q", got)
	}

	empty := &mcp.CallToolResult{IsError: true}
	if got := toolResultErrorMessage(empty); got != "" {
		t.Fatalf("expected empty string for no content, got %q", got)
	}

	if got := toolResultErrorMessage(nil); got != "" {
		t.Fatalf("expected empty string for nil result, got %q", got)
	}
}

// TestStripRouterOwnedIncludesFiltersActionSources pins that the
// router-only `web_search_call.action.sources` include entry is removed
// from the body forwarded upstream (inference servers reject unknown
// include values) while unrelated entries are preserved.
func TestStripRouterOwnedIncludesFiltersActionSources(t *testing.T) {
	body := map[string]any{"include": []any{"web_search_call.action.sources", "reasoning.encrypted_content"}}
	stripRouterOwnedIncludes(body)
	got, _ := body["include"].([]any)
	if len(got) != 1 || stringValue(got[0]) != "reasoning.encrypted_content" {
		t.Fatalf("expected only upstream-known include to remain, got %#v", got)
	}

	onlyRouter := map[string]any{"include": []any{"web_search_call.action.sources"}}
	stripRouterOwnedIncludes(onlyRouter)
	if _, present := onlyRouter["include"]; present {
		t.Fatalf("expected include to be dropped entirely when the only entry was router-owned, got %#v", onlyRouter)
	}

	empty := map[string]any{}
	stripRouterOwnedIncludes(empty)
	if _, present := empty["include"]; present {
		t.Fatal("stripRouterOwnedIncludes must not invent an include key")
	}
}
