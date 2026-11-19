//go:build localharness

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

func TestBoundedChatImageDispatch(t *testing.T) {
	const path = "/v1/chat/completions"
	const renderedTokens = 686 // Fake backend total for prompt text and image tokens.
	inline := boundedImageDataURL(t, "png")
	remote := "https://example.invalid/image.png"
	for _, tc := range []struct {
		name                           string
		url                            string
		cap                            int
		useProductionHandler           bool
		status                         int
		admissions, calls, generations int64
	}{
		{"at cap", inline, renderedTokens, false, http.StatusOK, 1, 1, 1},
		{"over cap", inline, renderedTokens - 1, false, http.StatusBadRequest, 1, 1, 0},
		{"remote rejected before admission", remote, renderedTokens, false, http.StatusBadRequest, 0, 0, 0},
		{"production still disabled", inline, renderedTokens, true, http.StatusBadRequest, 0, 0, 0},
		{"ordinary remote image unchanged", remote, 0, true, http.StatusOK, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var admissions, calls, generations atomic.Int64
			em, h := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				admissions.Add(1)
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body struct {
					InputCap  int `json:"max_input_tokens"`
					OutputCap int `json:"max_completion_tokens"`
					Count     int `json:"n"`
					Messages  []struct {
						Role    string `json:"role"`
						Content []struct {
							Type  string `json:"type"`
							Text  string `json:"text"`
							Image struct {
								URL    string `json:"url"`
								Detail string `json:"detail"`
							} `json:"image_url"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(body.Messages) != 1 || body.Messages[0].Role != "user" || len(body.Messages[0].Content) != 2 {
					t.Error("multimodal message shape changed")
				} else if text, image := body.Messages[0].Content[0], body.Messages[0].Content[1]; text.Type != "text" || text.Text != "describe" || image.Type != "image_url" || image.Image.URL != tc.url || image.Image.Detail != "high" {
					t.Error("text or image changed")
				}
				if body.InputCap != tc.cap || tc.cap != 0 && (body.OutputCap != 8 || body.Count != 1) {
					t.Errorf("wrong backend limits: input=%d, output=%d, count=%d", body.InputCap, body.OutputCap, body.Count)
				}
				w.Header().Set("Content-Type", "application/json")
				if body.InputCap != 0 && renderedTokens > body.InputCap {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":{"message":"rendered prompt exceeds cap","type":"invalid_request_error"}}`)
					return
				}
				generations.Add(1)
				io.WriteString(w, `{}`)
			}), "", false)
			body, err := json.Marshal(map[string]any{
				"model": admissionTestModel,
				"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "describe"}, boundedImagePart(tc.url),
				}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var r *http.Request
			if tc.cap != 0 {
				r = boundedRequest(path, string(body))
				r.Header.Set(maxInputTokensHeader, strconv.Itoa(tc.cap))
			} else {
				r = admissionRequest(path, string(body), "tk_test")
			}
			var handler http.Handler = h
			if !tc.useProductionHandler {
				handler = newRouterHandlerWithBoundedChat(em, h.client, func(_ string, selected *manager.Enclave) bool { return selected != nil })
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			h.client.refreshes.Wait()
			if w.Code != tc.status || admissions.Load() != tc.admissions || calls.Load() != tc.calls || generations.Load() != tc.generations {
				t.Fatalf("HTTP %d, admissions %d, dispatches %d, generations %d: %s", w.Code, admissions.Load(), calls.Load(), generations.Load(), w.Body.String())
			}
		})
	}
}
