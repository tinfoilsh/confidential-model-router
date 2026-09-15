//go:build localharness

package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/safeguards"
)

const realAPIKey = "tk_local_harness_api_key"

func chatJWT() string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"at+jwt"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user_local","client_id":"tinfoil-chat","product":"chat","exp":9999999999}`))
	return h + "." + p + ".sig"
}

type recordingSidecar struct {
	*httptest.Server
	mu   sync.Mutex
	subs []map[string]any
}

func newRecordingSidecar() *recordingSidecar {
	s := &recordingSidecar{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sub map[string]any
		json.NewDecoder(r.Body).Decode(&sub)
		s.mu.Lock()
		s.subs = append(s.subs, sub)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	return s
}

func (s *recordingSidecar) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.subs) }

// fakeUpstream mimics a vLLM enclave for chat completions and responses,
// streaming or not, and records the auth + conversation header it received.
func fakeUpstream(t *testing.T) (*httptest.Server, *[]http.Header) {
	var seen []http.Header
	var mu sync.Mutex
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		stream, _ := body["stream"].(bool)
		switch {
		case r.URL.Path == "/v1/chat/completions" && !stream:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": body["model"],
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Hello from the fake enclave."}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11}})
		case r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, tok := range []string{"Streamed ", "hello ", "world."} {
				fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": tok}}}})))
				fl.Flush()
			}
			fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}})))
			fmt.Fprint(w, "data: [DONE]\n\n")
		case r.URL.Path == "/v1/responses" && !stream:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "r1", "object": "response", "status": "completed",
				"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Responses reply."}}}},
				"usage":  map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}})
		case r.URL.Path == "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n", must(json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": "Streamed responses reply."})))
			fl.Flush()
			fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", must(json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed",
				"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Streamed responses reply."}}}},
				"usage":  map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}}})))
		default:
			http.NotFound(w, r)
		}
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{ecdsaCert(t)}}
	ts.StartTLS()
	return ts, &seen
}

// ecdsaCert mirrors real enclaves, whose fingerprinting only supports EC keys.
func ecdsaCert(t *testing.T) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func must(b []byte, err error) []byte { return b }

var (
	harnessOnce     sync.Once
	harnessManager  *manager.EnclaveManager
	harnessUpstream *httptest.Server
	harnessSeen     *[]http.Header
	harnessErr      error
)

// harness builds the EnclaveManager once per process: its constructor
// registers Prometheus collectors, which cannot be registered twice.
func harness(t *testing.T) (*manager.EnclaveManager, *httptest.Server, *[]http.Header) {
	t.Helper()
	harnessOnce.Do(func() {
		harnessUpstream, harnessSeen = fakeUpstream(t)
		cfg := []byte("models:\n  gpt-oss-120b:\n    repo: tinfoilsh/confidential-gpt-oss-120b\n")
		harnessManager, harnessErr = manager.NewEnclaveManager(cfg, "https://api.tinfoil.sh", "model-router", "s", "c", "d", "", "", time.Minute, true)
		if harnessErr != nil {
			return
		}
		harnessErr = manager.InstallFakeEnclaveForTest(harnessManager, "gpt-oss-120b", harnessUpstream)
		if harnessErr != nil {
			return
		}
		// Production enclaves present publicly trusted certificates and the
		// router pins their key on top. The fake upstream is self-signed, so
		// add its cert to the system roots for this process; the fingerprint
		// pin is still enforced by the proxy transport.
		pool, err := x509.SystemCertPool()
		if err != nil {
			harnessErr = err
			return
		}
		pool.AddCert(harnessUpstream.Certificate())
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	})
	if harnessErr != nil {
		t.Fatal(harnessErr)
	}
	return harnessManager, harnessUpstream, harnessSeen
}

func TestLocalRouter_EndToEnd(t *testing.T) {
	em, _, seen := harness(t)
	sidecar := newRecordingSidecar()
	defer sidecar.Close()

	submitter := safeguards.NewSubmitter(sidecar.URL)
	defer submitter.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := manager.BearerToken(r.Header.Get("Authorization"))
		w, capture, finish := submitter.Observe(w, r, manager.IsFirstPartyChatAccessJWT)
		defer finish()
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		if capture != nil {
			capture.SetMessages(safeguards.RequestMessages(r.URL.Path, body))
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		_ = apiKey
		model, _ := em.GetModel("gpt-oss-120b")
		enc, _ := model.NextEnclave(nil)
		if enc == nil {
			http.Error(w, "no enclave", 503)
			return
		}
		enc.ServeHTTP(w, r)
	})
	router := httptest.NewServer(handler)
	defer router.Close()

	type tc struct {
		name, path, auth, convID, body string
		wantReply                      string
		wantSubmit                     bool
	}
	cases := []tc{
		{"api key chat json", "/v1/chat/completions", realAPIKey, "conv-A", `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`, "Hello from the fake enclave.", false},
		{"api key chat stream", "/v1/chat/completions", realAPIKey, "", `{"model":"gpt-oss-120b","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Streamed hello world.", false},
		{"chat jwt chat json", "/v1/chat/completions", chatJWT(), "conv-B", `{"model":"gpt-oss-120b","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`, "Hello from the fake enclave.", true},
		{"chat jwt chat stream", "/v1/chat/completions", chatJWT(), "conv-C", `{"model":"gpt-oss-120b","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Streamed hello world.", true},
		{"chat jwt responses json", "/v1/responses", chatJWT(), "conv-D", `{"model":"gpt-oss-120b","instructions":"be brief","input":"hi"}`, "Responses reply.", true},
		{"chat jwt responses stream", "/v1/responses", chatJWT(), "conv-E", `{"model":"gpt-oss-120b","stream":true,"input":"hi"}`, "Streamed responses reply.", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := sidecar.count()
			seenBefore := len(*seen)
			req, _ := http.NewRequest("POST", router.URL+c.path, strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer "+c.auth)
			req.Header.Set("Content-Type", "application/json")
			if c.convID != "" {
				req.Header.Set(safeguards.ConversationIDHeader, c.convID)
			}
			start := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, raw)
			}
			if !strings.Contains(string(raw), strings.Fields(c.wantReply)[0]) {
				t.Fatalf("client did not receive the reply: %s", raw)
			}
			fmt.Fprintf(os.Stderr, "  %-28s HTTP %d  %5dB  %s\n", c.name, resp.StatusCode, len(raw), time.Since(start).Round(time.Millisecond))

			up := (*seen)[seenBefore]
			if up.Get(safeguards.ConversationIDHeader) != "" {
				t.Fatal("conversation id header leaked upstream")
			}
			if !strings.Contains(up.Get("Authorization"), c.auth) {
				t.Fatal("upstream did not receive the caller's bearer")
			}

			deadline := time.Now().Add(3 * time.Second)
			for sidecar.count() == before && c.wantSubmit && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(150 * time.Millisecond)
			got := sidecar.count() - before
			if c.wantSubmit && got != 1 {
				t.Fatalf("submissions = %d, want 1", got)
			}
			if !c.wantSubmit && got != 0 {
				t.Fatalf("API-key request produced %d submissions", got)
			}
			if c.wantSubmit {
				sidecar.mu.Lock()
				sub := sidecar.subs[len(sidecar.subs)-1]
				sidecar.mu.Unlock()
				if sub["credential"] != c.auth || sub["conversation_id"] != c.convID {
					t.Fatalf("submission attribution wrong: %v", sub)
				}
				msgs := sub["messages"].([]any)
				last := msgs[len(msgs)-1].(map[string]any)
				if last["role"] != "assistant" || last["content"] != c.wantReply {
					t.Fatalf("assistant turn wrong: %v", last)
				}
				fmt.Fprintf(os.Stderr, "      -> sidecar got %d msgs, conv=%q, last=%q\n", len(msgs), sub["conversation_id"], last["content"])
			}
		})
	}
	_ = bufio.NewReader
}

func TestLocalRouter_SurvivesSidecarOutage(t *testing.T) {
	em, _, _ := harness(t)

	// Sidecar URL points at a closed port: every delivery fails fast.
	submitter := safeguards.NewSubmitter("http://127.0.0.1:1")
	defer submitter.Close()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w, capture, finish := submitter.Observe(w, r, manager.IsFirstPartyChatAccessJWT)
		defer finish()
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		if capture != nil {
			capture.SetMessages(safeguards.RequestMessages(r.URL.Path, body))
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		model, _ := em.GetModel("gpt-oss-120b")
		enc, _ := model.NextEnclave(nil)
		enc.ServeHTTP(w, r)
	}))
	defer router.Close()

	var worst time.Duration
	for i := 0; i < 300; i++ {
		req, _ := http.NewRequest("POST", router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-oss-120b","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+chatJWT())
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if d := time.Since(start); d > worst {
			worst = d
		}
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
	fmt.Fprintf(os.Stderr, "  300 chat-JWT streaming requests with sidecar down: all 200, worst latency %s\n", worst.Round(time.Millisecond))
}
