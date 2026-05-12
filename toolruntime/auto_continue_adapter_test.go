package toolruntime

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestChatAdapterClassifiesAutoContinueCalls verifies the chat adapter
// partitions a turn's tool calls into router, auto-continue, and external
// buckets the way the loop driver expects.
func TestChatAdapterClassifiesAutoContinueCalls(t *testing.T) {
	adapter := newChatLoopAdapter(
		map[string]any{},
		nil,
		nil,
		map[string]struct{}{routerSearchToolName: {}},
		"m",
		http.Header{},
	)
	adapter.autoContinueTools = map[string]struct{}{"render_stat_cards": {}}

	response := &upstreamJSONResponse{
		body: map[string]any{
			"choices": []any{
				map[string]any{
					"index": float64(0),
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"id":   "c1",
								"type": "function",
								"function": map[string]any{
									"name":      routerSearchToolName,
									"arguments": "{}",
								},
							},
							map[string]any{
								"id":   "c2",
								"type": "function",
								"function": map[string]any{
									"name":      "render_stat_cards",
									"arguments": "{}",
								},
							},
							map[string]any{
								"id":   "c3",
								"type": "function",
								"function": map[string]any{
									"name":      "get_order",
									"arguments": "{}",
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
		},
	}

	router, display, hasExternal, _ := adapter.onUpstreamResponse(response, 0, time.Millisecond)
	if len(router) != 1 || router[0].name != routerSearchToolName {
		t.Fatalf("router classification wrong: %+v", router)
	}
	if len(display) != 1 || display[0].name != "render_stat_cards" {
		t.Fatalf("auto-continue classification wrong: %+v", display)
	}
	if !hasExternal {
		t.Fatal("expected hasExternal=true for a real client function call")
	}
}

// TestChatAdapterAutoContinueDoesNotFinalize checks the boundary case where
// a turn carries only router work and a auto-continue call: no external
// client tool call exists, so hasExternal must be false so the loop
// driver continues replaying.
func TestChatAdapterAutoContinueDoesNotFinalize(t *testing.T) {
	adapter := newChatLoopAdapter(
		map[string]any{},
		nil,
		nil,
		nil,
		"m",
		http.Header{},
	)
	adapter.autoContinueTools = map[string]struct{}{"render_chart": {}}

	response := &upstreamJSONResponse{
		body: map[string]any{
			"choices": []any{
				map[string]any{
					"index": float64(0),
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"id":   "c1",
								"type": "function",
								"function": map[string]any{
									"name":      "render_chart",
									"arguments": "{}",
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
		},
	}

	router, display, hasExternal, _ := adapter.onUpstreamResponse(response, 0, time.Millisecond)
	if len(router) != 0 {
		t.Fatalf("expected no router calls, got %+v", router)
	}
	if len(display) != 1 {
		t.Fatalf("expected one auto-continue call, got %+v", display)
	}
	if hasExternal {
		t.Fatal("auto-continue-only turn must not be treated as having external client calls")
	}
}

// TestChatAdapterStripsFlagFromUpstreamRequest verifies buildInitialRequest
// records the auto-continue set and removes the router-internal flag so the
// upstream model never sees it.
func TestChatAdapterStripsFlagFromUpstreamRequest(t *testing.T) {
	adapter := newChatLoopAdapter(
		map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
			"tools": []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":                         "render_stat_cards",
						"description":                  "kpis",
						"x-tinfoil-tool-auto-continue": true,
					},
				},
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        "get_order",
						"description": "real",
					},
				},
			},
		},
		nil,
		nil,
		nil,
		"m",
		http.Header{},
	)

	req := adapter.buildInitialRequest()

	if _, ok := adapter.autoContinueTools["render_stat_cards"]; !ok {
		t.Fatalf("adapter did not record auto-continue set: %+v", adapter.autoContinueTools)
	}

	tools, _ := req["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("expected tools to remain in upstream request body")
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if _, present := fn[autoContinueToolFlag]; present {
			t.Errorf("auto-continue flag leaked into upstream tools array on %v", fn["name"])
		}
	}
}

func TestChatAdapterCarriesAutoContinueCallsToFinalResponse(t *testing.T) {
	adapter := newChatLoopAdapter(map[string]any{}, nil, nil, nil, "m", http.Header{})
	state := map[string]any{
		"role": "assistant",
		"tool_calls": []any{
			map[string]any{
				"id":   "call_widget",
				"type": "function",
				"function": map[string]any{
					"name":      "render_stat_cards",
					"arguments": `{"stats":[]}`,
				},
			},
		},
	}
	items := adapter.autoContinueResponseItems(state, []toolCall{{id: "call_widget", name: "render_stat_cards"}})
	final := &upstreamJSONResponse{body: map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "Here is the explanation after the widget.",
				},
				"finish_reason": "stop",
			},
		},
	}}

	adapter.attachAutoContinueResponseItems(final, items)

	choice := final.body["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected auto-continue tool call on final response, got %#v", calls)
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if stringValue(fn["name"]) != "render_stat_cards" {
		t.Fatalf("expected render_stat_cards, got %#v", fn["name"])
	}
	if stringValue(message["content"]) == "" {
		t.Fatalf("expected final prose to remain present: %#v", message)
	}
}

func TestResponsesAdapterCarriesAutoContinueCallsToFinalResponse(t *testing.T) {
	adapter := newResponsesLoopAdapter(map[string]any{}, nil, nil, nil)
	state := []any{
		map[string]any{
			"id":        "fc_widget",
			"type":      "function_call",
			"name":      "render_stat_cards",
			"call_id":   "call_widget",
			"arguments": `{"stats":[]}`,
		},
	}
	items := adapter.autoContinueResponseItems(state, []toolCall{{id: "call_widget", name: "render_stat_cards"}})
	final := &upstreamJSONResponse{body: map[string]any{
		"output": []any{
			map[string]any{"id": "msg_final", "type": "message", "content": []any{}},
		},
	}}

	adapter.attachAutoContinueResponseItems(final, items)

	output := final.body["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected auto-continue call plus final output, got %#v", output)
	}
	first := output[0].(map[string]any)
	if stringValue(first["name"]) != "render_stat_cards" {
		t.Fatalf("expected auto-continue function_call first, got %#v", first)
	}
}

func TestChatAdapterMixedTurnCanonicalisesAutoContinueCalls(t *testing.T) {
	adapter := newChatLoopAdapter(map[string]any{}, nil, nil, nil, "m", http.Header{})
	adapter.autoContinueTools = map[string]struct{}{"render_stat_cards": {}}
	response := &upstreamJSONResponse{body: map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_widget",
							"type": "function",
							"function": map[string]any{
								"name":      "render_stat_cards",
								"arguments": `{"stats":"[{\"label\":\"a\",\"value\":1}]"}`,
							},
						},
						map[string]any{
							"id":   "call_external",
							"type": "function",
							"function": map[string]any{
								"name":      "get_order",
								"arguments": `{"id":"123"}`,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}}
	choice := response.body["choices"].([]any)[0].(map[string]any)
	state := choice["message"]
	items := adapter.autoContinueResponseItems(state, []toolCall{{id: "call_widget", name: "render_stat_cards"}})

	adapter.stripRouterToolCallsFromResponse(response)
	adapter.attachAutoContinueResponseItems(response, items)

	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("expected canonicalized render call plus external call, got %#v", calls)
	}
	widgetFn := calls[0].(map[string]any)["function"].(map[string]any)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stringValue(widgetFn["arguments"])), &parsed); err != nil {
		t.Fatalf("canonicalized arguments are invalid JSON: %v", err)
	}
	if _, ok := parsed["stats"].([]any); !ok {
		t.Fatalf("stats should be a native array, got %T", parsed["stats"])
	}
	externalFn := calls[1].(map[string]any)["function"].(map[string]any)
	if stringValue(externalFn["name"]) != "get_order" {
		t.Fatalf("expected external call to remain second, got %#v", externalFn)
	}
}

func TestResponsesAdapterMixedTurnCanonicalisesAutoContinueCalls(t *testing.T) {
	adapter := newResponsesLoopAdapter(map[string]any{}, nil, nil, nil)
	adapter.autoContinueTools = map[string]struct{}{"render_chart": {}}
	state := []any{
		map[string]any{
			"id":        "fc_widget",
			"type":      "function_call",
			"name":      "render_chart",
			"call_id":   "call_widget",
			"arguments": `{"series":"[{\"label\":\"Q1\",\"value\":10}]"}`,
		},
		map[string]any{
			"id":        "fc_external",
			"type":      "function_call",
			"name":      "get_order",
			"call_id":   "call_external",
			"arguments": `{"id":"123"}`,
		},
	}
	response := &upstreamJSONResponse{body: map[string]any{"output": append([]any{}, state...)}}
	items := adapter.autoContinueResponseItems(state, []toolCall{{id: "call_widget", name: "render_chart"}})

	adapter.stripRouterToolCallsFromResponse(response)
	adapter.attachAutoContinueResponseItems(response, items)

	output := response.body["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected canonicalized render item plus external item, got %#v", output)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stringValue(output[0].(map[string]any)["arguments"])), &parsed); err != nil {
		t.Fatalf("canonicalized arguments are invalid JSON: %v", err)
	}
	if _, ok := parsed["series"].([]any); !ok {
		t.Fatalf("series should be a native array, got %T", parsed["series"])
	}
	if stringValue(output[1].(map[string]any)["name"]) != "get_order" {
		t.Fatalf("expected external item to remain second, got %#v", output[1])
	}
}
