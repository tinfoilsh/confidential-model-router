//go:build localharness

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

func TestBoundedChatClientToolRoundTrip(t *testing.T) {
	const assistant = `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`
	const tools = `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]`
	for _, tc := range []struct{ name, content, result string }{
		{"explicit null and string result", `,"content":null`, `"two"`},
		{"omitted content and text parts result", "", `[{"type":"text","text":"two"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			em, h := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turn := calls.Add(1)
				var body struct {
					Messages []json.RawMessage `json:"messages"`
					Tools    []json.RawMessage `json:"tools"`
					InputCap int               `json:"max_input_tokens"`
					Count    int               `json:"n"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.InputCap != 48 || body.Count != 1 || len(body.Tools) != 1 {
					t.Errorf("unexpected forwarded request: %+v, %v", body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch turn {
				case 1:
					if len(body.Messages) != 1 {
						t.Errorf("first turn: expected one message, got %d", len(body.Messages))
					}
					io.WriteString(w, `{"choices":[{"message":`+assistant+`,"finish_reason":"tool_calls"}]}`)
				case 2:
					if len(body.Messages) != 3 || !strings.Contains(string(body.Messages[1]), `"tool_calls"`) || !strings.Contains(string(body.Messages[2]), `"tool_call_id":"call_1"`) {
						t.Errorf("second turn missing client tool history: %s", body.Messages)
					}
					io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"two"},"finish_reason":"stop"}]}`)
				default:
					t.Error("unexpected additional backend dispatch")
				}
			}), "", false)
			handler := newRouterHandlerWithBoundedChat(em, h.client, func(_ string, replica *manager.Enclave) bool { return replica != nil })
			const prefix = `{"model":"gpt-oss-120b","tools":` + tools + `,"tool_choice":{"type":"function","function":{"name":"lookup"}},"messages":[{"role":"user","content":"look up one"}`
			path := "/v1/chat/completions"
			first := httptest.NewRecorder()
			handler.ServeHTTP(first, boundedRequest(path, prefix+` ]}`))
			var output struct {
				Choices []struct {
					Message json.RawMessage `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(first.Body.Bytes(), &output); first.Code != http.StatusOK || err != nil || len(output.Choices) != 1 || calls.Load() != 1 {
				t.Fatalf("first turn: HTTP %d, dispatches %d: %s", first.Code, calls.Load(), first.Body.String())
			}
			returned := strings.Replace(string(output.Choices[0].Message), `,"content":null`, tc.content, 1)
			second := httptest.NewRecorder()
			handler.ServeHTTP(second, boundedRequest(path, prefix+`,`+returned+`,{"role":"tool","tool_call_id":"call_1","content":`+tc.result+`}]}`))
			if second.Code != http.StatusOK || calls.Load() != 2 || !strings.Contains(second.Body.String(), `"content":"two"`) {
				t.Fatalf("second turn: HTTP %d, dispatches %d: %s", second.Code, calls.Load(), second.Body.String())
			}
		})
	}
}

func TestBoundedChatRejectsUnlistedShapesWithoutChangingOrdinaryChat(t *testing.T) {
	var admissions, dispatches atomic.Int64
	em, h := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admissions.Add(1)
		io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body["future_extension"]) != "true" {
			t.Errorf("ordinary chat extension was not forwarded: %s, %v", body, err)
		}
		io.WriteString(w, `{}`)
	}), "", false)
	handler := newRouterHandlerWithBoundedChat(em, h.client, func(_ string, replica *manager.Enclave) bool { return replica != nil })
	const path = "/v1/chat/completions"
	const base = `{"model":"gpt-oss-120b",`
	for _, tc := range []struct{ name, fields, param string }{
		{"unknown extension", `"messages":[{"role":"user","content":"hi"}],"future_extension":true`, "future_extension"},
		{"stream extension", `"messages":[{"role":"user","content":"hi"}],"stream_options":{"extra":true}`, "stream_options"},
		{"non-function tool", `"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"mcp"}]`, "tools"},
		{"function definition extension", `"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","server":{}}}]`, "tools"},
		{"non-function choice", `"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"web_search"}`, "tool_choice"},
		{"non-function history", `"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"mcp"}]}]`, "messages"},
		{"empty call history", `"messages":[{"role":"assistant","content":null,"tool_calls":[]}]`, "messages"},
		{"null tool result", `"messages":[{"role":"tool","tool_call_id":"t","content":null}]`, "messages"},
		{"nested image extension", `"messages":[{"role":"tool","tool_call_id":"t","content":[{"type":"text","text":"hi","image_url":{"url":"https://example.com"}}]}]`, "messages"},
		{"assistant audio", `"messages":[{"role":"assistant","content":"hi","audio":{"id":"a"}}]`, "messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, boundedRequest(path, base+tc.fields+"}"))
			h.client.refreshes.Wait()
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"param":"`+tc.param+`"`) || admissions.Load() != 0 || dispatches.Load() != 0 {
				t.Fatalf("HTTP %d, admissions %d, dispatches %d: %s", w.Code, admissions.Load(), dispatches.Load(), w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, admissionRequest(path, base+`"messages":[{"role":"user","content":"hi"}],"future_extension":true}`, "tk_test"))
	if w.Code != http.StatusOK || dispatches.Load() != 1 {
		t.Fatalf("ordinary chat: HTTP %d, dispatches %d: %s", w.Code, dispatches.Load(), w.Body.String())
	}
}
