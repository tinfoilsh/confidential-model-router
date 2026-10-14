//go:build localharness

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	webSearchTestSearchTool  = "router_search"
	webSearchTestFetchTool   = "router_fetch"
	webSearchTestModelMaxAge = 0
	webSearchTestCacheOnly   = -1
	webSearchTestAnswer      = "final answer"
)

// TestRouterExternalWebAccess drives a web search request through the real
// router and a real MCP session, asserting what the model is offered and
// what the search tool receives.
func TestRouterExternalWebAccess(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, tc := range []struct {
			name       string
			disabled   bool
			wantFetch  bool
			wantMaxAge float64
		}{
			{name: "default", wantFetch: true, wantMaxAge: webSearchTestModelMaxAge},
			{name: "disabled", disabled: true, wantFetch: false, wantMaxAge: webSearchTestCacheOnly},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				var mu sync.Mutex
				var offered []string
				var searchArgs map[string]any
				var fetchCalls, turns atomic.Int64

				server := mcp.NewServer(&mcp.Implementation{Name: "websearch", Version: "test"}, nil)
				server.AddTool(&mcp.Tool{Name: "search", InputSchema: &jsonschema.Schema{Type: "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					mu.Lock()
					defer mu.Unlock()
					if err := json.Unmarshal(req.Params.Arguments, &searchArgs); err != nil {
						t.Error(err)
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no results"}}}, nil
				})
				server.AddTool(&mcp.Tool{Name: "fetch", InputSchema: &jsonschema.Schema{Type: "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					fetchCalls.Add(1)
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "page"}}}, nil
				})
				mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

				_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
				}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/mcp" {
						mcpHandler.ServeHTTP(w, r)
						return
					}
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					turn := turns.Add(1)
					if turn == 1 {
						mu.Lock()
						offered = webSearchTestToolNames(request["tools"])
						mu.Unlock()
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(webSearchTestModelResponse(path, turn == 1))
				}), "", false)

				encoded, err := json.Marshal(webSearchTestRequest(path, tc.disabled))
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, admissionRequest(path, string(encoded), "tk_test"))
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), webSearchTestAnswer) {
					t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
				}

				mu.Lock()
				defer mu.Unlock()
				hasTool := map[string]bool{}
				for _, name := range offered {
					hasTool[name] = true
				}
				if !hasTool[webSearchTestSearchTool] {
					t.Errorf("model was not offered %s: %v", webSearchTestSearchTool, offered)
				}
				if hasTool[webSearchTestFetchTool] != tc.wantFetch {
					t.Errorf("model offered %s = %v, want %v: %v", webSearchTestFetchTool, hasTool[webSearchTestFetchTool], tc.wantFetch, offered)
				}
				if searchArgs["max_age_hours"] != tc.wantMaxAge {
					t.Errorf("search max_age_hours = %v, want %v", searchArgs["max_age_hours"], tc.wantMaxAge)
				}
				if turns.Load() != 2 || fetchCalls.Load() != 0 {
					t.Errorf("model turns/fetch calls = %d/%d, want 2/0", turns.Load(), fetchCalls.Load())
				}
			})
		}
	}
}

func webSearchTestRequest(path string, disabled bool) map[string]any {
	webSearch := map[string]any{}
	if disabled {
		webSearch["external_web_access"] = false
	}
	body := map[string]any{"model": admissionTestModel}
	if path == "/v1/chat/completions" {
		body["messages"] = []any{map[string]any{"role": "user", "content": "news"}}
		body["web_search_options"] = webSearch
		return body
	}
	webSearch["type"] = "web_search"
	body["input"] = "news"
	body["tools"] = []any{webSearch}
	return body
}

// webSearchTestModelResponse plays the model: the first turn asks for a
// search that requests a live crawl, the second turn answers.
func webSearchTestModelResponse(path string, search bool) map[string]any {
	const arguments = `{"query":"news","max_age_hours":0}`
	if path == "/v1/chat/completions" {
		message := map[string]any{"role": "assistant", "content": webSearchTestAnswer}
		finish := "stop"
		if search {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": webSearchTestSearchTool, "arguments": arguments}}}
			finish = "tool_calls"
		}
		return map[string]any{"id": "chat-1", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
	}
	output := map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": webSearchTestAnswer}}}
	if search {
		output = map[string]any{"type": "function_call", "call_id": "call-1", "name": webSearchTestSearchTool, "arguments": arguments, "status": "completed"}
	}
	return map[string]any{"id": "resp-1", "status": "completed", "output": []any{output}}
}

func webSearchTestToolNames(raw any) []string {
	tools, _ := raw.([]any)
	names := make([]string, 0, len(tools))
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		if function, ok := tool["function"].(map[string]any); ok {
			tool = function
		}
		if name, _ := tool["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}
