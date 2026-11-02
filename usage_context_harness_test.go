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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinfoilsh/confidential-model-router/manager"
	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const usageContextTestKey = "tk_test"

// harnessUsageContext signs the usage context the chat harness attaches to
// each of a run's model calls.
func harnessUsageContext(t *testing.T, bill bool, secret string) http.Header {
	t.Helper()
	h := http.Header{}
	if err := usagereporting.SetHeaders(h, usagereporting.Context{
		ContextID:           "run-1",
		RootRequestID:       "run-1",
		ParentService:       "harness",
		APIKeyHash:          usagereporting.HashAPIKey(usageContextTestKey),
		Depth:               1,
		BillCustomerRequest: bill,
		IssuedAt:            time.Now(),
	}, secret); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestUsageContextSetsCustomerRequests sends harness model calls through the
// real router, on the plain proxy and the tool loop, streamed and not, and
// checks the customer requests their single billing event carries.
func TestUsageContextSetsCustomerRequests(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "usage-tools", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "search"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "search result"}}}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	for _, route := range []struct{ name, body string }{
		{"proxy", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`},
		{"proxy streaming", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"tool loop", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hi"}],"web_search_options":{}}`},
		{"tool loop streaming", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hi"}],"web_search_options":{},"stream":true}`},
	} {
		for _, tc := range []struct {
			name    string
			usage   func(t *testing.T, secret string) http.Header
			want    int64
			refused bool
		}{
			{name: "no usage context", usage: func(*testing.T, string) http.Header { return nil }, want: 1},
			{name: "harness turn already billed", usage: func(t *testing.T, secret string) http.Header { return harnessUsageContext(t, false, secret) }, want: 0},
			{name: "harness turn billed", usage: func(t *testing.T, secret string) http.Header { return harnessUsageContext(t, true, secret) }, want: 1},
			{name: "forged usage context", usage: func(t *testing.T, _ string) http.Header { return harnessUsageContext(t, false, "forged-secret") }, refused: true},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				var admissions, models atomic.Int64
				var mu sync.Mutex
				var events []usagereporting.Event
				em, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case routeContextPath:
						admissions.Add(1)
						io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
					case usagereporting.IngestionPath:
						var batch usagereporting.Batch
						if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
							t.Error(err)
						}
						mu.Lock()
						events = append(events, batch.Events...)
						mu.Unlock()
					default:
						t.Errorf("unexpected CP path %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/mcp" {
						mcpHandler.ServeHTTP(w, r)
						return
					}
					models.Add(1)
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request["stream"] == true {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, `data: {"id":"up_1","created":1700000001,"model":"gpt-oss-120b","choices":[{"index":0,"delta":{"role":"assistant","content":"final answer"}}]}`+"\n\n")
						io.WriteString(w, `data: {"id":"up_1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n")
						io.WriteString(w, "data: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"up_1","choices":[{"index":0,"message":{"role":"assistant","content":"final answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
				}), "", false)
				stopBilling := manager.EnableBillingForTest(em)

				r := admissionRequest("/v1/chat/completions", route.body, usageContextTestKey)
				for key, values := range tc.usage(t, em.UsageContextSecret()) {
					r.Header[key] = values
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				handler.client.refreshes.Wait()
				stopBilling()

				mu.Lock()
				defer mu.Unlock()
				if tc.refused {
					if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "usage context") {
						t.Fatalf("forged usage context: HTTP %d: %s", rec.Code, rec.Body.String())
					}
					if admissions.Load() != 0 || models.Load() != 0 || len(events) != 0 {
						t.Fatalf("forged usage context reached admission=%d models=%d billing=%d", admissions.Load(), models.Load(), len(events))
					}
					return
				}
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "final answer") {
					t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
				}
				if len(events) != 1 {
					t.Fatalf("billing events = %d, want 1: %+v", len(events), events)
				}
				event := events[0]
				if event.CustomerRequests != tc.want || event.APIKey != usageContextTestKey {
					t.Fatalf("customer requests = %d for %q, want %d", event.CustomerRequests, event.APIKey, tc.want)
				}
				if _, nested := event.Attributes["parent_service"]; nested {
					t.Fatalf("event marked nested, which the control plane never counts: %+v", event.Attributes)
				}
			})
		}
	}
}
