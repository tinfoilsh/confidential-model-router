//go:build localharness

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/cacheroute"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

const admissionTestOrg = "org_admission_test"

type admissionBackendRequest struct {
	path          string
	body          []byte
	header        http.Header
	contentLength int64
}

func newAdmissionHarness(t *testing.T, cp, backend http.Handler, org string, overloaded bool) (*manager.EnclaveManager, http.Handler) {
	t.Helper()
	models := []string{admissionTestModel, "nomic-embed-text", "qwen3-tts", "voxtral-small-24b", "voxtral-mini-4b-realtime", "doc-upload", "websearch"}
	var cfg strings.Builder
	cfg.WriteString("models:\n")
	for _, name := range models {
		fmt.Fprintf(&cfg, "  %s: {repo: org/test}\n", name)
	}
	return newAdmissionHarnessWithConfig(t, cp, backend, org, overloaded, []byte(cfg.String()))
}

func newAdmissionHarnessWithConfig(t *testing.T, cp, backend http.Handler, org string, overloaded bool, cfg []byte) (*manager.EnclaveManager, http.Handler) {
	t.Helper()
	control := httptest.NewServer(cp)
	t.Cleanup(control.Close)
	upstream := httptest.NewUnstartedServer(backend)
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{ecdsaCert(t)}}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	base := http.DefaultTransport
	transport := base.(*http.Transport).Clone()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	http.DefaultTransport = transport
	t.Cleanup(func() { transport.CloseIdleConnections(); http.DefaultTransport = base })
	em, err := manager.NewAdmissionManagerForTest(cfg, control.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(em.Shutdown)
	for name := range em.Models() {
		if err := manager.InstallFakeEnclaveForTest(em, name, upstream); err != nil {
			t.Fatal(err)
		}
		if err := manager.ConfigureAdmissionModelForTest(em, name, org, overloaded); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.ConfigureAdmissionModelForTest(em, admissionTestModel, org, overloaded); err != nil {
		t.Fatal(err)
	}
	return em, newRouterHandler(em, newRouteContextClient(control.URL), nil)
}

func TestAdmissionHarnessOverloadRecovery(t *testing.T) {
	var backends atomic.Int64
	em, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backends.Add(1)
		io.WriteString(w, `{}`)
	}), "", false)
	const unknownModel = "unknown-model"
	if err := manager.ConfigureAdmissionModelForTest(em, unknownModel, "", false); err == nil || !strings.Contains(err.Error(), unknownModel) {
		t.Fatalf("unconfigured model error = %v", err)
	}
	for _, overloaded := range []bool{true, false, true, false} {
		if err := manager.ConfigureAdmissionModelForTest(em, admissionTestModel, "", overloaded); err != nil {
			t.Fatal(err)
		}
		before := backends.Load()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[]}`, "tk_test"))
		wantStatus, wantBackends := http.StatusOK, int64(1)
		if overloaded {
			wantStatus, wantBackends = http.StatusServiceUnavailable, 0
		}
		if rec.Code != wantStatus || backends.Load()-before != wantBackends {
			t.Fatalf("overloaded=%v: HTTP=%d backend calls=%d: %s", overloaded, rec.Code, backends.Load()-before, rec.Body.String())
		}
		model, _ := em.GetModel(admissionTestModel)
		for host, enclave := range model.Enclaves {
			if _, _, configured := enclave.OverloadMarks(); configured != overloaded {
				t.Fatalf("overloaded=%v: thresholds configured=%v", overloaded, configured)
			}
			if !overloaded && testutil.ToFloat64(manager.BackendOverloaded.WithLabelValues(admissionTestModel, host)) != 0 {
				t.Fatal("healthy enclave still reports overload")
			}
		}
	}
}

func TestAdmissionHarnessConfiguredCacheRoute(t *testing.T) {
	previous := *cacheSaltEnabled
	*cacheSaltEnabled = true
	t.Cleanup(func() { *cacheSaltEnabled = previous })
	for _, mode := range []cacheroute.Mode{cacheroute.ModeShadow, cacheroute.ModeEnforced} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := fmt.Sprintf("models:\n  %s:\n    repo: org/test\n    enclaves: [replica-a, replica-b]\n    cache_route: {mode: %s, min_prompt_bytes: 1}\n", admissionTestModel, mode)
			var admissions, backends atomic.Int64
			_, handler := newAdmissionHarnessWithConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				admissions.Add(1)
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backends.Add(1)
				io.WriteString(w, `{}`)
			}), "", false, []byte(cfg))
			keyed := cacheroute.RequestsTotal.WithLabelValues(admissionTestModel, string(cacheroute.OutcomeKeyed))
			failed := cacheroute.RequestsTotal.WithLabelValues(admissionTestModel, string(cacheroute.OutcomeError))
			warm := cacheroute.ReuseTotal.WithLabelValues(admissionTestModel, cacheroute.ReuseRepeatWarm)
			beforeKeyed, beforeFailed, beforeWarm := testutil.ToFloat64(keyed), testutil.ToFloat64(failed), testutil.ToFloat64(warm)
			const requests = 2
			for range requests {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hello"}]}`, "tk_test"))
				if rec.Code != http.StatusOK {
					t.Fatalf("cache-route dispatch: HTTP %d: %s", rec.Code, rec.Body.String())
				}
			}
			if admissions.Load() != requests || backends.Load() != requests {
				t.Fatalf("admissions=%d backends=%d", admissions.Load(), backends.Load())
			}
			if got := testutil.ToFloat64(keyed) - beforeKeyed; got != requests {
				t.Errorf("keyed dispatches=%v, want %d", got, requests)
			}
			if got := testutil.ToFloat64(failed) - beforeFailed; got != 0 {
				t.Errorf("recovered cache-route errors=%v", got)
			}
			if got := testutil.ToFloat64(warm) - beforeWarm; got != requests-1 {
				t.Errorf("warm repeats=%v, want %d", got, requests-1)
			}
		})
	}
}

func TestAdmissionHandlerUnknownModel(t *testing.T) {
	const unknownModel = "unknown-model"
	var admissions, backends atomic.Int64
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admissions.Add(1)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backends.Add(1)
	}), "", false)
	{
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":%q,"messages":[]}`, unknownModel)
		handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", body, "tk_test"))
		var envelope manager.ErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusNotFound || envelope.Error.Code == nil || *envelope.Error.Code != manager.ErrCodeModelNotFound {
			t.Fatalf("unknown model: HTTP %d: %s", rec.Code, rec.Body.String())
		}
	}
	if admissions.Load() != 0 || backends.Load() != 0 {
		t.Fatalf("unknown model reached admission/backend: %d/%d", admissions.Load(), backends.Load())
	}
}

func admissionRequest(path, body, key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

func TestAdmissionHandlerEntryPoints(t *testing.T) {
	var multipartBody bytes.Buffer
	mw := multipart.NewWriter(&multipartBody)
	mw.WriteField("model", "voxtral-small-24b")
	part, _ := mw.CreateFormFile("file", "audio.wav")
	part.Write([]byte("\x00\xffaudio"))
	mw.Close()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	zw.Write([]byte(`[{"jsonrpc":"2.0","method":"tools/list","id":9007199254740993}]`))
	zw.Close()
	cases := []struct {
		name, path, body, model, contentType, encoding string
		raw, upgrade                                   bool
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"gpt-oss-120b","priority":-99,"messages":[]}`, model: admissionTestModel},
		{name: "responses", path: "/v1/responses", body: `{"model":"gpt-oss-120b","priority":-99,"input":"hi"}`, model: admissionTestModel},
		{name: "completions", path: "/v1/completions", body: `{"model":"gpt-oss-120b","priority":-99,"prompt":"hi"}`, model: admissionTestModel},
		{name: "auto", path: "/v1/chat/completions", body: `{"model":"auto","messages":[]}`, model: admissionTestModel},
		{name: "embeddings", path: "/v1/embeddings", body: `{"model":"nomic-embed-text","priority":-99,"input":"hi"}`, model: "nomic-embed-text"},
		{name: "speech default", path: "/v1/audio/speech", body: `{"input":"hi","priority":-99}`, model: "qwen3-tts"},
		{name: "speech explicit", path: "/v1/audio/speech", body: `{"model":"gpt-oss-120b","input":"hi"}`, model: admissionTestModel},
		{name: "transcription", path: "/v1/audio/transcriptions", body: multipartBody.String(), model: "voxtral-small-24b", contentType: mw.FormDataContentType(), raw: true},
		{name: "translation", path: "/v1/audio/translations", body: multipartBody.String(), model: "voxtral-small-24b", contentType: mw.FormDataContentType(), raw: true},
		{name: "file convert", path: "/v1/convert/file", body: "\x00\xfffile", model: "doc-upload", contentType: "application/octet-stream", raw: true},
		{name: "realtime", path: "/v1/realtime?model=gpt-oss-120b", model: admissionTestModel, upgrade: true},
		{name: "realtime default", path: "/v1/realtime?intent=transcription", model: "voxtral-mini-4b-realtime", upgrade: true},
		{name: "MCP batch", path: "/mcp", body: `[{"jsonrpc":"2.0","method":"tools/list","id":9007199254740993}]`, model: "websearch", raw: true},
		{name: "MCP compressed", path: "/mcp", body: compressed.String(), model: "websearch", encoding: "gzip", raw: true},
	}
	for _, decision := range []string{decisionAllowed, decisionDemote, decisionExempt, decisionRejected} {
		t.Run(decision, func(t *testing.T) {
			var cpCalls, backendCalls atomic.Int64
			seen := make(chan admissionBackendRequest, len(cases))
			admissions := make(chan routeContextRequest, len(cases))
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request routeContextRequest
				json.NewDecoder(r.Body).Decode(&request)
				admissions <- request
				cpCalls.Add(1)
				priority := ""
				if decision == decisionExempt {
					priority = `,"priority":-2`
				}
				reason := ""
				if decision == decisionDemote || decision == decisionRejected {
					reason = rateReasonRequests
				}
				fmt.Fprintf(w, `{"org_id":%q%s,"rate_limit":{"decision":%q,"reason":%q,"retry_after_seconds":12}}`, admissionTestOrg, priority, decision, reason)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backendCalls.Add(1)
				data, _ := io.ReadAll(r.Body)
				seen <- admissionBackendRequest{r.URL.Path, data, r.Header.Clone(), r.ContentLength}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"result":"backend reached"}`)
			}), admissionTestOrg, false)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					beforeCP, beforeBackend := cpCalls.Load(), backendCalls.Load()
					r := admissionRequest(tc.path, tc.body, "tk_test")
					r.Header.Set("X-Tinfoil-Root-Request-Id", "client-chosen-id")
					if tc.contentType != "" {
						r.Header.Set("Content-Type", tc.contentType)
					}
					if tc.encoding != "" {
						r.Header.Set("Content-Encoding", tc.encoding)
					}
					if tc.upgrade {
						r.Method = http.MethodGet
						r.Header.Set("Connection", "Upgrade")
						r.Header.Set("Upgrade", "websocket")
						r.Header.Del("Authorization")
						r.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.tk_test")
					}
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, r)
					wantStatus := 200
					if decision == decisionRejected {
						wantStatus = 429
					}
					if rec.Code != wantStatus {
						t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
					}
					if cpCalls.Load()-beforeCP != 1 {
						t.Fatalf("admission count = %d", cpCalls.Load()-beforeCP)
					}
					request := <-admissions
					if request.Model != tc.model || request.APIKey != "tk_test" {
						t.Fatalf("wrong admission: %+v", request)
					}
					if decision == decisionRejected {
						if backendCalls.Load() != beforeBackend || rec.Header().Get("Retry-After") != "12" {
							t.Fatal("rejection reached backend or lost retry hint")
						}
						return
					}
					if backendCalls.Load()-beforeBackend != 1 {
						t.Fatalf("backend count = %d", backendCalls.Load()-beforeBackend)
					}
					forwarded := <-seen
					if forwarded.header.Get("Authorization") != "Bearer tk_test" {
						t.Fatal("original auth lost")
					}
					if tc.upgrade {
						if forwarded.header.Get("Upgrade") != "websocket" || forwarded.header.Get("Sec-WebSocket-Protocol") != "realtime" {
							t.Fatalf("upgrade headers: %v", forwarded.header)
						}
						return
					}
					if tc.raw {
						if !bytes.Equal(forwarded.body, []byte(tc.body)) || forwarded.header.Get("Content-Encoding") != tc.encoding {
							t.Fatal("opaque body or encoding corrupted")
						}
						return
					}
					var body map[string]json.RawMessage
					if err := json.Unmarshal(forwarded.body, &body); err != nil {
						t.Fatal(err)
					}
					priority := ""
					if cacheSaltPaths[r.URL.Path] && decision == decisionDemote {
						priority = "1"
					}
					if cacheSaltPaths[r.URL.Path] && decision == decisionExempt {
						priority = "-2"
					}
					if string(body["priority"]) != priority {
						t.Fatalf("priority=%s, want %q", body["priority"], priority)
					}
					if forwarded.contentLength != int64(len(forwarded.body)) {
						t.Fatal("wrong rewritten content length")
					}
					if tc.name == "auto" && string(body["model"]) != `"gpt-oss-120b"` {
						t.Fatalf("auto not resolved: %s", forwarded.body)
					}
				})
			}
		})
	}
}

func TestAdmissionHandlerRejectsBeforePreprocessing(t *testing.T) {
	var backendCalls, cpCalls atomic.Int64
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpCalls.Add(1)
		io.WriteString(w, `{"rate_limit":{"decision":"rejected","reason":"tokens","retry_after_seconds":9}}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backendCalls.Add(1) }), "", false)
	for _, body := range []string{
		`{"model":"gpt-oss-120b","input":[{"role":"user","content":[{"type":"input_file","filename":"test.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]}`,
		`{"model":"gpt-oss-120b","input":"hi","tools":[{"type":"web_search"}]}`,
		`{"model":"gpt-oss-120b","input":"hi","tools":[{"type":"function","name":"show","parameters":{"type":"object"},"x-tinfoil-tool-auto-continue":true}]}`,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, admissionRequest("/v1/responses", body, "tk_test"))
		if rec.Code != 429 {
			t.Fatalf("reject HTTP %d: %s", rec.Code, rec.Body.String())
		}
	}
	if backendCalls.Load() != 0 || cpCalls.Load() != 3 {
		t.Fatalf("preprocessing after rejection: backend=%d cp=%d", backendCalls.Load(), cpCalls.Load())
	}
}

// The backend fixture accepts the shaped token to test forwarding, not JWT
// verification. Downstream rejection is tested separately below.
func TestAdmissionHandlerJWTClassificationAndMissingAuth(t *testing.T) {
	var cpCalls, backendCalls atomic.Int64
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		if r.Header.Get("Authorization") == "" {
			t.Error("auth stripped")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"count":7,"result":"ok"}`)
	}), "", false)
	jwt := accessTokenForTest(`{"alg":"EdDSA","typ":"at+jwt"}`, `{"sub":"user_test"}`)
	for _, path := range []string{"/v1/chat/completions", chatInputTokensPath, responsesInputTokensPath, "/v1/audio/speech"} {
		for range 3 {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest(path, `{"model":"gpt-oss-120b","messages":[],"input":"hi"}`, jwt))
			if rec.Code != 200 {
				t.Fatalf("JWT HTTP %d: %s", rec.Code, rec.Body.String())
			}
		}
	}
	if cpCalls.Load() != 0 || backendCalls.Load() != 12 {
		t.Fatalf("JWT admission calls=%d backends=%d", cpCalls.Load(), backendCalls.Load())
	}
	for _, key := range []string{"", "opaque", accessTokenForTest(`{"typ":"JWT"}`, `{"sub":"user"}`), "a.b.c", jwt + "!"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[]}`, key))
		if rec.Code != 401 {
			t.Fatalf("invalid auth accepted: %d", rec.Code)
		}
	}
	if backendCalls.Load() != 12 || cpCalls.Load() != 4 {
		t.Fatalf("invalid auth bypass: backend=%d cp=%d", backendCalls.Load(), cpCalls.Load())
	}
}

func TestAdmissionHandlerMetadataOnly(t *testing.T) {
	var calls atomic.Int64
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["model"]; ok {
			t.Error("input tokens consumed admission")
		}
		fmt.Fprintf(w, `{"org_id":%q}`, admissionTestOrg)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenizePath {
			t.Errorf("unexpected backend path %s", r.URL.Path)
		}
		io.WriteString(w, `{"count":7}`)
	}), admissionTestOrg, false)
	for _, path := range []string{chatInputTokensPath, responsesInputTokensPath} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, admissionRequest(path, `{"model":"auto","messages":[],"input":"hi"}`, "tk_test"))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"input_tokens":7`) {
			t.Fatalf("metadata count: %d %s", rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("metadata calls=%d", calls.Load())
	}
}

func TestAdmissionHandlerOverloadPriorityExemption(t *testing.T) {
	for _, tc := range []struct {
		decision, priority string
		status             int
	}{
		{decisionAllowed, "", 503}, {decisionDemote, "", 503},
		{decisionExempt, "", 200}, {decisionExempt, `,"priority":-1`, 200},
		{decisionAllowed, `,"priority":0`, 200},
	} {
		t.Run(tc.decision+tc.priority, func(t *testing.T) {
			var calls atomic.Int64
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reason := ""
				if tc.decision == decisionDemote {
					reason = rateReasonRequests
				}
				fmt.Fprintf(w, `{"rate_limit":{"decision":%q,"reason":%q,"retry_after_seconds":0}%s}`, tc.decision, reason, tc.priority)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{}`) }), "", true)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[]}`, "tk_test"))
			if rec.Code != tc.status {
				t.Fatalf("overload priority: HTTP %d: %s", rec.Code, rec.Body.String())
			}
			if tc.status == 503 && (calls.Load() != 0 || !strings.Contains(rec.Body.String(), "server_is_overloaded")) {
				t.Fatal("overload protection lost")
			}
			if tc.status == 200 && calls.Load() != 1 {
				t.Fatal("exempt did not reach backend")
			}
		})
	}
}

func TestAdmissionHandlerFileAndToolDispatchCountOnce(t *testing.T) {
	for _, jwt := range []bool{false, true} {
		t.Run(fmt.Sprint("jwt=", jwt), func(t *testing.T) {
			var cpCalls, delegates, conversions, modelCalls, toolCalls atomic.Int64
			var orderMu sync.Mutex
			var order []string
			record := func(event string) { orderMu.Lock(); order = append(order, event); orderMu.Unlock() }
			server := mcp.NewServer(&mcp.Implementation{Name: "admission-tools", Version: "1"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "search"}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
				Query string `json:"query"`
			}) (*mcp.CallToolResult, any, error) {
				toolCalls.Add(1)
				record("tool")
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "search result"}}}, nil, nil
			})
			mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
			org := admissionTestOrg
			if jwt {
				org = ""
			}
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/internal/inference/delegate" {
					delegates.Add(1)
					json.NewEncoder(w).Encode(map[string]any{"access_token": "delegated-token", "grant_token": "grant", "access_token_expires_at": time.Now().Add(time.Hour), "grant_expires_at": time.Now().Add(time.Hour)})
					return
				}
				cpCalls.Add(1)
				record("admit")
				var req routeContextRequest
				json.NewDecoder(r.Body).Decode(&req)
				if req.Model != admissionTestModel {
					t.Errorf("counted internal model: %q", req.Model)
				}
				fmt.Fprintf(w, `{"org_id":%q,"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`, org)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/mcp" {
					mcpHandler.ServeHTTP(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/convert/file" {
					conversions.Add(1)
					record("convert")
					io.WriteString(w, `{"document":{"md_content":"converted document"}}`)
					return
				}
				modelCalls.Add(1)
				record("model")
				data, _ := io.ReadAll(r.Body)
				if !bytes.Contains(data, []byte("converted document")) {
					t.Error("model ran before file conversion")
				}
				if jwt && r.Header.Get("Authorization") != "Bearer delegated-token" {
					t.Error("delegation lost on internal model call")
				}
				if modelCalls.Load() == 1 {
					io.WriteString(w, `{"id":"first","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"router_search","arguments":"{\"query\":\"hello\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
				} else {
					io.WriteString(w, `{"id":"last","choices":[{"index":0,"message":{"role":"assistant","content":"final answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
				}
			}), org, false)
			key := "tk_test"
			if jwt {
				key = accessTokenForTest(`{"alg":"EdDSA","typ":"at+jwt"}`, `{"sub":"user_test","client_id":"tinfoil-chat","product":"chat"}`)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"test.pdf","file_data":"data:application/pdf;base64,JVBERi0="}}]}],"web_search_options":{}}`, key))
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "final answer") {
				t.Fatalf("tool loop HTTP %d: %s", rec.Code, rec.Body.String())
			}
			wantCP, wantDelegate := int64(1), int64(0)
			if jwt {
				wantCP, wantDelegate = 0, 1
			}
			if cpCalls.Load() != wantCP || delegates.Load() != wantDelegate || conversions.Load() != 1 || modelCalls.Load() != 2 || toolCalls.Load() != 1 {
				t.Fatalf("calls cp=%d delegates=%d files=%d models=%d tools=%d", cpCalls.Load(), delegates.Load(), conversions.Load(), modelCalls.Load(), toolCalls.Load())
			}
			orderMu.Lock()
			defer orderMu.Unlock()
			wantOrder := "admit,convert,model,tool,model"
			if jwt {
				wantOrder = "convert,model,tool,model"
			}
			if strings.Join(order, ",") != wantOrder {
				t.Fatalf("dispatch order: %v", order)
			}
		})
	}
}

func TestAdmissionHandlerSharedDecisions(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	tokensUsed := 0
	var calls, backends atomic.Int64
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req routeContextRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.APIKey != "tk_account_a" && req.APIKey != "tk_account_b" {
			t.Error("unexpected account credential")
		}
		mu.Lock()
		defer mu.Unlock()
		counts[req.Model]++
		decision, reason := decisionAllowed, ""
		if counts[req.Model] == 2 {
			decision, reason = decisionDemote, rateReasonRequests
		}
		if counts[req.Model] > 2 {
			decision, reason = decisionRejected, rateReasonRequests
		}
		if tokensUsed >= 100 {
			decision, reason = decisionRejected, rateReasonTokens
		}
		fmt.Fprintf(w, `{"rate_limit":{"decision":%q,"reason":%q,"retry_after_seconds":10,"requests":{"limit":2,"used":%d},"tokens":{"limit":100,"used":%d}}}`, decision, reason, counts[req.Model], tokensUsed)
	})
	em, first := newAdmissionHarness(t, control, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backends.Add(1)
		io.Copy(w, r.Body)
	}), "", false)
	secondControl := httptest.NewServer(control)
	defer secondControl.Close()
	second := newRouterHandler(em, newRouteContextClient(secondControl.URL), nil)
	for i, tc := range []struct {
		handler    http.Handler
		key, model string
		status     int
		message    string
	}{
		{first, "tk_account_a", admissionTestModel, 200, ""},
		{second, "tk_account_b", admissionTestModel, 200, `"priority":1`},
		{first, "tk_account_b", admissionTestModel, 429, "for requests"},
		{second, "tk_account_a", "nomic-embed-text", 200, ""},
		{second, "tk_account_a", "nomic-embed-text", 429, "for tokens"},
	} {
		if i == 4 {
			mu.Lock()
			tokensUsed = 100
			mu.Unlock()
		}
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":%q,"messages":[]}`, tc.model)
		tc.handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", body, tc.key))
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.message) {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 5 || backends.Load() != 3 {
		t.Fatalf("cached or repeated admission: CP=%d backend=%d", calls.Load(), backends.Load())
	}
}

func TestAdmissionHandlerJWTBypassDoesNotOverrideDownstreamRejection(t *testing.T) {
	forged := accessTokenForTest(`{"alg":"EdDSA","typ":"at+jwt"}`, `{"sub":"claimed-user"}`)
	var lookups, downstream atomic.Int64
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstream.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+forged {
			t.Error("downstream did not receive original unverified token")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid JWT signature","type":"authentication_error"}}`)
	}), "", false)
	for _, path := range []string{"/v1/chat/completions", chatInputTokensPath} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, admissionRequest(path, `{"model":"gpt-oss-120b","messages":[]}`, forged))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Invalid JWT signature") {
			t.Fatalf("downstream rejection overridden: %d %s", rec.Code, rec.Body.String())
		}
	}
	if lookups.Load() != 0 || downstream.Load() != 2 {
		t.Fatalf("quota bypass/verification hop: lookups=%d downstream=%d", lookups.Load(), downstream.Load())
	}
}

func TestAdmissionHandlerRealtimeUpgrade(t *testing.T) {
	var calls atomic.Int64
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nready")
		rw.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(rw, payload); err != nil {
			t.Error(err)
			return
		}
		if string(payload) != "ping" {
			t.Errorf("upgrade data=%q", payload)
		}
		io.WriteString(rw, "pong")
		rw.Flush()
	}), "", false)
	router := httptest.NewServer(handler)
	defer router.Close()
	r, _ := http.NewRequest(http.MethodGet, router.URL+"/v1/realtime?model="+admissionTestModel, nil)
	r.Header.Set("Authorization", "Bearer tk_test")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &http.Client{}
	resp, err := client.Do(r.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 101 {
		t.Fatalf("upgrade HTTP %d", resp.StatusCode)
	}
	ready := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, ready); err != nil || string(ready) != "ready" {
		t.Fatalf("upgrade read: %q %v", ready, err)
	}
	writer, ok := resp.Body.(io.Writer)
	if !ok {
		t.Fatal("upgraded connection is not writable")
	}
	if _, err := io.WriteString(writer, "ping"); err != nil {
		t.Fatal(err)
	}
	pong := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, pong); err != nil || string(pong) != "pong" {
		t.Fatalf("upgrade roundtrip: %q %v", pong, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upgrade admission calls=%d", calls.Load())
	}
}

func TestAdmissionHandlerFailuresAndEmptyOrg(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		upstream, want int
		reserved       bool
	}{
		{"unauthorized", `{"error":"invalid key"}`, 401, 401, false},
		{"payment", `{"error":"payment required"}`, 402, 402, false},
		{"forbidden", `{"error":"forbidden"}`, 403, 403, false},
		{"outage", `{}`, 500, 200, false},
		{"missing decision", `{}`, 200, 200, false},
		{"invalid decision", `{"rate_limit":{"decision":"other","retry_after_seconds":0}}`, 200, 200, false},
		{"negative retry", `{"rate_limit":{"decision":"rejected","retry_after_seconds":-1}}`, 200, 200, false},
		{"empty org", `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`, 200, 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, backends atomic.Int64
			org := ""
			if tc.reserved {
				org = admissionTestOrg
			}
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.upstream)
				io.WriteString(w, tc.response)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backends.Add(1)
				io.WriteString(w, `{}`)
			}), org, false)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[]}`, "tk_test"))
			// A lookup the control plane could not answer admits the request
			// to the shared pool; only its verdicts stop it short of a backend.
			wantBackends := int64(0)
			if tc.want == http.StatusOK {
				wantBackends = 1
			}
			if rec.Code != tc.want || calls.Load() != 1 || backends.Load() != wantBackends {
				t.Fatalf("failure dispatch: HTTP=%d CP=%d backend=%d %s", rec.Code, calls.Load(), backends.Load(), rec.Body.String())
			}
		})
	}
}

func TestAdmissionHandlerConcurrentRequests(t *testing.T) {
	const requests = 32
	var calls, backends atomic.Int64
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backends.Add(1)
		io.WriteString(w, `{}`)
	}), "", false)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[]}`, "tk_test"))
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent admission: HTTP %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	wg.Wait()
	if calls.Load() != requests || backends.Load() != requests {
		t.Fatalf("concurrent admission calls=%d backend=%d, want %d each", calls.Load(), backends.Load(), requests)
	}
}

func TestAdmissionHandlerMetadataFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls, backends atomic.Int64
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req map[string]any
				json.NewDecoder(r.Body).Decode(&req)
				if _, ok := req["model"]; ok {
					t.Error("metadata failure retried as admission")
				}
				w.WriteHeader(status)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backends.Add(1)
				io.WriteString(w, `{"count":1}`)
			}), "", false)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest(chatInputTokensPath, `{"model":"gpt-oss-120b","messages":[]}`, "tk_test"))
			// A control plane outage on the metadata lookup still counts
			// tokens; only credential denials stop the request.
			want, wantBackends := status, int64(0)
			if status == http.StatusInternalServerError {
				want, wantBackends = http.StatusOK, 1
			}
			if rec.Code != want || calls.Load() != 1 || backends.Load() != wantBackends {
				t.Fatalf("metadata failure: HTTP=%d CP=%d backend=%d: %s", rec.Code, calls.Load(), backends.Load(), rec.Body.String())
			}
		})
	}
}

func TestAdmissionHandlerQuotaDenial(t *testing.T) {
	for _, tc := range quotaRetryAfterCases {
		t.Run(tc.name, func(t *testing.T) {
			var calls, backends atomic.Int64
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for _, value := range tc.values {
					w.Header().Add("Retry-After", value)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, quotaDenialTestBody)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backends.Add(1) }), "", false)
			for _, path := range []string{"/v1/chat/completions", chatInputTokensPath} {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, admissionRequest(path, `{"model":"gpt-oss-120b","messages":[]}`, "tk_test"))
				assertQuotaDenial(t, rec, tc.want)
			}
			if calls.Load() != 2 || backends.Load() != 0 {
				t.Fatalf("quota denial dispatch: CP=%d backend=%d", calls.Load(), backends.Load())
			}
		})
	}
}

func TestModelHostHeaderIsIgnored(t *testing.T) {
	var admissions atomic.Int64
	admitted := make(chan routeContextRequest, 4)
	_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request routeContextRequest
		json.NewDecoder(r.Body).Decode(&request)
		admitted <- request
		admissions.Add(1)
		io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}), "", false)

	// A model label in the forwarded host must not select the model: only
	// the body does, and a body without one is rejected the same way.
	withHost := func(r *http.Request) *http.Request {
		r.Header.Set("X-Forwarded-Host", "nomic-embed-text.localhost")
		return r
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, withHost(admissionRequest("/v1/chat/completions", `{"model":"gpt-oss-120b","messages":[]}`, "tk_test")))
	if rec.Code != http.StatusOK {
		t.Fatalf("host-labelled chat: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if request := <-admitted; request.Model != admissionTestModel {
		t.Fatalf("host label selected the model: %+v", request)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, withHost(admissionRequest("/v1/chat/completions", `{"messages":[]}`, "tk_test")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("host label supplied a missing model: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, withHost(httptest.NewRequest(http.MethodGet, "/health", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("host-labelled health: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if admissions.Load() != 1 {
		t.Fatalf("admissions = %d, want 1", admissions.Load())
	}
}
