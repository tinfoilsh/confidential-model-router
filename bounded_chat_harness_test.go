//go:build localharness

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

func boundedRequest(path, body string) *http.Request {
	r := admissionRequest(path, body, "tk_test")
	r.Header.Set(singleCallRequestHeader, "true")
	r.Header.Set(maxInputTokensHeader, "48")
	r.Header.Set(manager.MaxOutputTokensRequestHeader, "8")
	r.Header.Set(manager.CompletionCountRequestHeader, "1")
	return r
}

const boundedBody = `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hello"}]}`

func TestBoundedChatRejectsBeforeAdmission(t *testing.T) {
	var admissions, dispatches atomic.Int64
	em, h := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admissions.Add(1)
		io.WriteString(w, `{"rate_limit":{"decision":"allowed"}}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		io.WriteString(w, `{}`)
	}), "", false)
	withBackend := newRouterHandlerWithBoundedChat(em, h.client, func(_ string, selected *manager.Enclave) bool { return selected != nil })
	for _, tc := range []struct {
		name, path, body, message string
		change                    func(*http.Request)
		defaultHandler            bool
	}{
		{name: "wrong route", path: "/v1/responses", message: "only supports POST"},
		{name: "wrong decoded route", message: "only supports POST", change: func(r *http.Request) { r.URL.Path = "/v1/responses"; r.URL.RawPath = "/v1/chat/completions" }},
		{name: "wrong method", message: "only supports POST", change: func(r *http.Request) { r.Method = http.MethodGet }},
		{name: "encoded route", path: "/v1/chat/%63ompletions", message: "only supports POST"},
		{name: "mode duplicate", message: "single true", change: func(r *http.Request) { r.Header.Add(singleCallRequestHeader, "true") }},
		{name: "input without mode", message: "requires", change: func(r *http.Request) { r.Header.Del(singleCallRequestHeader) }},
		{name: "input absent", message: "single positive integer", change: func(r *http.Request) { r.Header.Del(maxInputTokensHeader) }},
		{name: "input ambiguous", message: "single positive integer", change: func(r *http.Request) { r.Header.Set(maxInputTokensHeader, "1, 8") }},
		{name: "output absent", message: "single positive integer", change: func(r *http.Request) { r.Header.Del(manager.MaxOutputTokensRequestHeader) }},
		{name: "count absent", message: "must be 1", change: func(r *http.Request) { r.Header.Del(manager.CompletionCountRequestHeader) }},
		{name: "count two", message: "must be 1", change: func(r *http.Request) { r.Header.Set(manager.CompletionCountRequestHeader, "2") }},
		{name: "hop by hop cap", message: "Connection", change: func(r *http.Request) { r.Header.Add("Connection", "keep-alive, x-tinfoil-max-input-tokens") }},
		{name: "search", body: `,"web_search_options":{}`, message: "server-side tools"},
		{name: "file", body: `,"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"x.pdf","file_data":"eA=="}}]}]`, message: "text message content"},
		{name: "image", body: `,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]`, message: "text message content"},
		{name: "audio output modality", body: `,"modalities":["text","audio"]`, message: "text output"},
		{name: "audio output options", body: `,"audio":{"voice":"alloy","format":"wav"}`, message: "text output"},
		{name: "best of", body: `,"best_of":1`, message: "best_of"},
		{name: "beam", body: `,"use_beam_search":true`, message: "use_beam_search"},
		{name: "truncate", body: `,"truncate_prompt_tokens":5`, message: "truncate_prompt_tokens"},
		{name: "kv transfer", body: `,"kv_transfer_params":{}`, message: "kv_transfer_params"},
		{name: "auto", body: `,"model":"auto"`, message: "explicit model"},
		{name: "no enforcing backend", message: "not supported on this model", defaultHandler: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			r := boundedRequest(path, strings.TrimSuffix(boundedBody, "}")+tc.body+"}")
			if tc.change != nil {
				tc.change(r)
			}
			handler := withBackend
			if tc.defaultHandler {
				handler = h
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.message) {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			h.client.refreshes.Wait()
			if admissions.Load() != 0 || dispatches.Load() != 0 {
				t.Fatal("rejected request reached admission or a backend")
			}
		})
	}
}

func TestBoundedChatBackendContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  string
		want int
	}{
		{"ordinary", "", http.StatusOK},
		{"at input limit", "5", http.StatusOK},
		{"above input limit", "4", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dispatches, generations atomic.Int64
			em, h := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dispatches.Add(1)
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if tc.cap == "" {
					if _, present := body[maxInputTokensField]; present {
						t.Error("private field from caller reached ordinary chat backend")
					}
				} else {
					if r.Header.Get(maxInputTokensHeader) != "" || r.Header.Get(singleCallRequestHeader) != "" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
						t.Error("bounded dispatch retained router-only or idempotency headers")
					}
					if string(body[maxInputTokensField]) != tc.cap || string(body["n"]) != "1" || string(body["max_completion_tokens"]) != "8" || body["max_tokens"] != nil {
						t.Errorf("backend limits not canonical: %s", body)
					}
					// Model fixture: assume the rendered prompt is five tokens.
					if tc.cap == "4" {
						http.Error(w, "rendered prompt exceeds cap", http.StatusBadRequest)
						return
					}
				}
				generations.Add(1)
				io.WriteString(w, `{}`)
			}), "", false)
			body := strings.TrimSuffix(boundedBody, "}") + `,"max_input_tokens":999999,"n":3,"max_tokens":500}`
			r := admissionRequest("/v1/chat/completions", body, "tk_test")
			if tc.cap != "" {
				r = boundedRequest("/v1/chat/completions", body)
				r.Header.Set(maxInputTokensHeader, tc.cap)
				r.Header.Set("Idempotency-Key", "key")
				r.Header.Set("X-Idempotency-Key", "other-key")
			}
			w := httptest.NewRecorder()
			newRouterHandlerWithBoundedChat(em, h.client, func(model string, selected *manager.Enclave) bool {
				return model == admissionTestModel && selected != nil
			}).ServeHTTP(w, r)
			if w.Code != tc.want || dispatches.Load() != 1 || (generations.Load() == 1) != (tc.want == http.StatusOK) {
				t.Fatalf("HTTP %d, dispatches %d, generations %d: %s", w.Code, dispatches.Load(), generations.Load(), w.Body.String())
			}
		})
	}
}

func TestBoundedChatDoesNotReplay502(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var primary, alternate atomic.Int64
			em, h := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"org_id":%q,"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`, admissionTestOrg)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primary.Add(1)
				http.Error(w, "engine unavailable", http.StatusBadGateway)
			}), admissionTestOrg, false)
			model, _ := em.GetModel(admissionTestModel)
			var firstHost string
			for host := range model.Enclaves {
				firstHost = host
			}
			secondHost := addAdmissionBackend(t, em, admissionTestModel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				alternate.Add(1)
				io.WriteString(w, `{}`)
			}))
			if err := manager.ConfigureReservationsForTest(em, admissionTestModel, model.Reservations, []string{firstHost, secondHost}); err != nil {
				t.Fatal(err)
			}
			body := strings.TrimSuffix(boundedBody, "}") + fmt.Sprintf(`,"stream":%t}`, stream)
			w := httptest.NewRecorder()
			newRouterHandlerWithBoundedChat(em, h.client, func(_ string, selected *manager.Enclave) bool { return selected != nil }).ServeHTTP(w, boundedRequest("/v1/chat/completions", body))
			if w.Code != http.StatusBadGateway || primary.Load() != 1 || alternate.Load() != 0 {
				t.Fatalf("HTTP %d, primary %d, alternate %d: %s", w.Code, primary.Load(), alternate.Load(), w.Body.String())
			}
		})
	}
}

func TestBoundedChatRoutesOnlyToCapableBackend(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(fmt.Sprintf("capable=%t", capable), func(t *testing.T) {
			var admissions, unsupported, supported atomic.Int64
			em, h := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				admissions.Add(1)
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				unsupported.Add(1)
				io.WriteString(w, `{}`)
			}), "", false)
			host := addAdmissionBackend(t, em, admissionTestModel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				supported.Add(1)
				io.WriteString(w, `{}`)
			}))
			handler := newRouterHandlerWithBoundedChat(em, h.client, func(name string, selected *manager.Enclave) bool {
				return capable && name == admissionTestModel && selected.String() == host
			})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, boundedRequest("/v1/chat/completions", boundedBody))
			h.client.refreshes.Wait()
			if capable {
				if w.Code != http.StatusOK || admissions.Load() != 1 || supported.Load() != 1 || unsupported.Load() != 0 {
					t.Fatalf("HTTP %d, admissions %d, capable dispatches %d, unsupported dispatches %d", w.Code, admissions.Load(), supported.Load(), unsupported.Load())
				}
			} else if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || admissions.Load() != 0 || supported.Load() != 0 || unsupported.Load() != 0 {
				t.Fatalf("HTTP %d, headers %v, admissions %d, dispatches %d/%d", w.Code, w.Header(), admissions.Load(), supported.Load(), unsupported.Load())
			}
		})
	}
}
