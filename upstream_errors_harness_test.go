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

func TestRouterPreservesDelegationErrors(t *testing.T) {
	const privateDetail = "private delegation detail"
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var delegationCalls, backendCalls atomic.Int64
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/internal/inference/delegate" {
					t.Errorf("unexpected control-plane request: %s", r.URL.Path)
				}
				delegationCalls.Add(1)
				w.Header().Set("Retry-After", "42")
				w.WriteHeader(status)
				io.WriteString(w, privateDetail)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backendCalls.Add(1)
			}), "", false)
			key := accessTokenForTest(`{"alg":"EdDSA","typ":"at+jwt"}`, `{"sub":"user_test","client_id":"tinfoil-chat","product":"chat"}`)
			for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
				toolOptions := `"web_search_options":{}`
				if path == "/v1/responses" {
					toolOptions = `"tools":[{"type":"web_search"}]`
				}
				for _, stream := range []bool{false, true} {
					body := fmt.Sprintf(`{"model":%q,"messages":[],"input":"hi",%s,"stream":%t}`, admissionTestModel, toolOptions, stream)
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, admissionRequest(path, body, key))
					if rec.Code != status || rec.Header().Get("Retry-After") != "42" {
						t.Errorf("%s stream=%t: HTTP %d retry=%q: %s", path, stream, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
					}
					if strings.Contains(rec.Body.String(), privateDetail) {
						t.Fatal("private delegation detail leaked")
					}
				}
			}
			if delegationCalls.Load() != 4 || backendCalls.Load() != 0 {
				t.Fatalf("delegation/backend calls = %d/%d", delegationCalls.Load(), backendCalls.Load())
			}
		})
	}
}

func TestRouterPreservesDocumentErrors(t *testing.T) {
	for _, tc := range []struct {
		status  int
		errType string
	}{
		{http.StatusPaymentRequired, "insufficient_quota"},
		{http.StatusTooManyRequests, "insufficient_quota"},
		{http.StatusTooManyRequests, manager.ErrTypeRateLimit},
		{http.StatusInternalServerError, manager.ErrTypeServer},
		{http.StatusServiceUnavailable, manager.ErrTypeServiceUnavailable},
		{http.StatusGatewayTimeout, manager.ErrTypeServer},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.errType), func(t *testing.T) {
			var conversions, inference atomic.Int64
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/convert/file" {
					inference.Add(1)
					return
				}
				conversions.Add(1)
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":{"message":"Document rejected","type":%q,"code":"fixture_error"}}`, tc.errType)
			}), "", false)
			for _, endpoint := range []struct{ path, body string }{
				{"/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"test.pdf","file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`},
				{"/v1/responses", `{"model":"gpt-oss-120b","input":[{"role":"user","content":[{"type":"input_file","filename":"test.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]}`},
			} {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, admissionRequest(endpoint.path, endpoint.body, "tk_test"))
				if rec.Code != tc.status || rec.Header().Get("Retry-After") != "30" {
					t.Errorf("%s: HTTP %d retry=%q: %s", endpoint.path, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
				}
				var body manager.ErrorEnvelope
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Type != tc.errType || body.Error.Code == nil || *body.Error.Code != "fixture_error" {
					t.Errorf("error classification lost: %+v", body.Error)
				}
			}
			if conversions.Load() != 2 || inference.Load() != 0 {
				t.Fatalf("conversion/inference calls = %d/%d", conversions.Load(), inference.Load())
			}
		})
	}
}

func TestRouterPreservesUnreadableDocumentErrorStatus(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversized=%t", oversized), func(t *testing.T) {
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "30")
				if !oversized {
					w.Header().Set("Content-Length", "100")
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				if oversized {
					io.WriteString(w, strings.Repeat("x", manager.MaxUpstreamErrorBodyBytes+1))
				} else {
					io.WriteString(w, "truncated")
				}
			}), "", false)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest("/v1/responses", `{"model":"gpt-oss-120b","input":[{"role":"user","content":[{"type":"input_file","filename":"test.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]}`, "tk_test"))
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
				t.Fatalf("lost status/retry on unreadable error: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
}
