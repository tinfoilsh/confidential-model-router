//go:build localharness

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/toolruntime"
	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const (
	billingTestPromptTokens  = 11
	billingTestOutputTokens  = 7
	billingTestCachedTokens  = 3
	billingTestToolBudget    = 10
	billingTestToolName      = "router_search"
	billingTestToolArguments = `{"query":"hello"}`
	billingTestPrecisionSeed = json.Number("9007199254740993") // 2^53+1 cannot be represented by float64.
)

func billingLoopBody(path, model string) map[string]any {
	if path == "/v1/chat/completions" {
		return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "web_search_options": map[string]any{}}
	}
	return map[string]any{"model": model, "input": "hi", "tools": []any{map[string]any{"type": "web_search"}}}
}

func billingLoopResponse(path string, turn int, terminal bool, usageMode string) map[string]any {
	response := map[string]any{"id": fmt.Sprintf("turn-%d", turn)}
	usage := map[string]any{}
	if path == "/v1/chat/completions" {
		message := map[string]any{"role": "assistant", "content": "final answer"}
		finish := "stop"
		if !terminal {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": fmt.Sprintf("call-%d", turn), "type": "function", "function": map[string]any{"name": billingTestToolName, "arguments": billingTestToolArguments}}}
			finish = "tool_calls"
		}
		response["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}
		usage = map[string]any{"prompt_tokens": billingTestPromptTokens, "completion_tokens": billingTestOutputTokens, "prompt_tokens_details": map[string]any{"cached_tokens": billingTestCachedTokens}}
	} else {
		output := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "final answer"}}}
		if !terminal {
			output = map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call-%d", turn), "name": billingTestToolName, "arguments": billingTestToolArguments}
		}
		response["status"] = "completed"
		output["status"] = "completed"
		response["output"] = []any{output}
		usage = map[string]any{"input_tokens": billingTestPromptTokens, "output_tokens": billingTestOutputTokens, "input_tokens_details": map[string]any{"cached_tokens": billingTestCachedTokens}}
	}
	usage["total_tokens"] = billingTestPromptTokens + billingTestOutputTokens
	if usageMode == "zero" {
		usage = map[string]any{"total_tokens": 0}
	}
	if usageMode != "missing" {
		response["usage"] = usage
	}
	return response
}

// billingReplyText returns the assistant reply of a completed non-streaming
// chat or Responses body, or "" when the body is not a completed reply.
func billingReplyText(path string, body []byte) string {
	var reply struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return ""
	}
	if path == "/v1/chat/completions" {
		if len(reply.Choices) == 0 {
			return ""
		}
		return reply.Choices[0].Message.Content
	}
	if reply.Status != "completed" {
		return ""
	}
	var text strings.Builder
	for _, item := range reply.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

func TestNonstreamToolBillingCompletedUsage(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "billing-tools", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "search"}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Query string `json:"query"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "search result"}}}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, tc := range []struct {
			name, outcome, usage string
			completed            int
			router               bool
		}{
			{name: "first turn error", outcome: "error"},
			{name: "later error", outcome: "error", completed: 1},
			{name: "later cancellation", outcome: "cancel", completed: 1},
			{name: "unconsumed truncated usage", outcome: "truncated", completed: 1},
			{name: "zero usage then error", outcome: "error", completed: 1, usage: "zero"},
			{name: "missing usage then error", outcome: "error", completed: 1, usage: "missing"},
			{name: "success once", outcome: "success", completed: 1},
			{name: "zero usage success", outcome: "success", completed: 1, usage: "zero"},
			{name: "multiple turns then error", outcome: "error", completed: 3},
			{name: "multiple turns success", outcome: "success", completed: 3},
			{name: "forced final error", outcome: "error", completed: billingTestToolBudget},
			{name: "forced final success", outcome: "success", completed: billingTestToolBudget + 1},
			{name: "canonical auto later error", outcome: "error", completed: 1, router: true},
			{name: "canonical auto success", outcome: "success", completed: 3, router: true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var turns, admissions atomic.Int64
				var mu sync.Mutex
				var events []usagereporting.Event
				em, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case routeContextPath:
						admissions.Add(1)
						var req routeContextRequest
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						if req.Model != admissionTestModel {
							t.Errorf("admission model = %q", req.Model)
						}
						io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
					case usagereporting.IngestionPath:
						var batch usagereporting.Batch
						if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
							t.Error(err)
						}
						mu.Lock()
						events = append(events, batch.Events...)
						mu.Unlock()
						w.WriteHeader(http.StatusOK)
					default:
						t.Errorf("unexpected CP path %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/mcp" {
						mcpHandler.ServeHTTP(w, r)
						return
					}
					turn := int(turns.Add(1))
					var request map[string]any
					decoder := json.NewDecoder(r.Body)
					decoder.UseNumber()
					if err := decoder.Decode(&request); err != nil {
						t.Error(err)
					}
					if request["seed"] != billingTestPrecisionSeed {
						t.Errorf("seed precision lost on turn %d: %v", turn, request["seed"])
					}
					if r.URL.Path != path || request["model"] != admissionTestModel || request["stream"] != false {
						t.Errorf("unexpected loop dispatch: %s %#v", r.URL.Path, request)
					}
					if turn > billingTestToolBudget {
						if _, exists := request["tools"]; exists {
							t.Error("forced-final request still contains tools")
						}
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Request-Id", fmt.Sprintf("completed-%d", turn))
					w.Header().Set("Tinfoil-Enclave", fmt.Sprintf("enclave-%d", turn))
					if turn > tc.completed {
						switch tc.outcome {
						case "cancel":
							cancel()
							<-r.Context().Done()
						case "truncated":
							io.WriteString(w, `{"usage":{"prompt_tokens":999,"completion_tokens":999},`)
						default:
							w.Header().Set("Retry-After", "17")
							w.WriteHeader(http.StatusBadGateway)
							io.WriteString(w, `{"error":{"message":"second turn failed","type":"server_error","code":"fixture_failure"},"usage":{"prompt_tokens":999,"completion_tokens":999}}`)
						}
						return
					}
					json.NewEncoder(w).Encode(billingLoopResponse(path, turn, tc.outcome == "success" && turn == tc.completed, tc.usage))
				}), "", false)
				stopBilling := manager.EnableBillingForTest(em)
				body := billingLoopBody(path, admissionTestModel)
				body["seed"] = billingTestPrecisionSeed
				if tc.router {
					body["model"] = "auto"
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				r := admissionRequest(path, string(encoded), "tk_test").WithContext(ctx)
				r.Header.Set("X-Request-Id", "original-request")
				rec := httptest.NewRecorder()
				if tc.router {
					handler.ServeHTTP(rec, r)
					handler.client.refreshes.Wait()
					if admissions.Load() != 1 {
						t.Fatalf("admissions = %d", admissions.Load())
					}
				} else {
					err = toolruntime.Handle(rec, r, em, []toolruntime.Profile{toolruntime.WebSearch}, body, admissionTestModel, &toolruntime.RouterOptions{})
				}
				stopBilling()
				switch tc.outcome {
				case "success":
					if err != nil || rec.Code != 200 || !strings.Contains(rec.Body.String(), "final answer") {
						t.Fatalf("success changed: %v HTTP %d %s", err, rec.Code, rec.Body.String())
					}
					if got := billingReplyText(path, rec.Body.Bytes()); got != "final answer" {
						t.Fatalf("completed reply not delivered: %q", got)
					}
				case "error":
					if err != nil || rec.Code != 502 || !strings.Contains(rec.Body.String(), "fixture_failure") || rec.Header().Get("Retry-After") != "17" {
						t.Fatalf("original upstream failure changed: %v HTTP %d %s", err, rec.Code, rec.Body.String())
					}
				case "cancel":
					if !errors.Is(err, context.Canceled) || rec.Body.Len() != 0 {
						t.Fatalf("original cancellation lost: %v %s", err, rec.Body.String())
					}
				case "truncated":
					var syntaxErr *json.SyntaxError
					if !errors.As(err, &syntaxErr) || rec.Body.Len() != 0 {
						t.Fatalf("original decode error lost: %v %s", err, rec.Body.String())
					}
				}
				wantTurns := tc.completed
				if tc.outcome != "success" {
					wantTurns++
				}
				if turns.Load() != int64(wantTurns) {
					t.Fatalf("model turns = %d, want %d", turns.Load(), wantTurns)
				}
				wantEvents := 1
				if tc.outcome != "success" && (tc.completed == 0 || tc.usage != "") {
					wantEvents = 0
				}
				mu.Lock()
				defer mu.Unlock()
				if len(events) != wantEvents {
					t.Fatalf("billing events = %d, want %d: %+v", len(events), wantEvents, events)
				}
				if wantEvents == 0 {
					return
				}
				event := events[0]
				if event.Attributes["model"] != admissionTestModel || event.Attributes["route"] != path || event.Attributes["streaming"] != "false" || event.Attributes["enclave"] != fmt.Sprintf("enclave-%d", tc.completed) || event.RequestID != fmt.Sprintf("completed-%d", tc.completed) || event.APIKey != "tk_test" || event.CustomerRequests != 1 || event.Operation.Name != usagereporting.OperationRouterModelRequest || event.Operation.Service != usagereporting.ServiceRouter {
					t.Fatalf("billing attribution: %+v", event)
				}
				meters := map[string]int64{}
				for _, meter := range event.Meters {
					meters[meter.Name] += meter.Quantity
				}
				billedTurns := int64(tc.completed)
				if tc.usage != "" {
					billedTurns = 0
				}
				if meters[usagereporting.MeterInputTokens] != billedTurns*billingTestPromptTokens || meters[usagereporting.MeterOutputTokens] != billedTurns*billingTestOutputTokens || meters[usagereporting.MeterCachedInputTokens] != billedTurns*billingTestCachedTokens {
					t.Fatalf("completed-turn usage lost or duplicated: %+v", meters)
				}
			})
		}
	}
}
