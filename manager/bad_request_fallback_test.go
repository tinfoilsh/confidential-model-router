package manager

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/tinfoil-go/verify"
)

const (
	fallbackTestModel = "gpt-oss-120b"
	fallbackTestBody  = `{"model":"gpt-oss-120b","input":"Reply OK.","stream":true,"mm_processor_kwargs":{"max_soft_tokens":280}}`
	fallbackTestError = `{"error":{"message":"Per-request options disabled","type":"invalid_request_error","param":"mm_processor_kwargs"}}`
)

func fallbackTestEnclave(t *testing.T, handler http.HandlerFunc) *Enclave {
	t.Helper()
	server, key := renewalServer(t, handler)
	e := newTestEnclave(strings.TrimPrefix(server.URL, "https://"))
	e.modelName, e.tlsKeyFP = fallbackTestModel, key
	e.attestation = newAttestationState(time.Now().Add(time.Hour), time.Now(), "v1")
	collector := billing.NewCollector("", "", "")
	t.Cleanup(collector.Stop)
	e.proxy = newProxy(e.host, key, e.modelName, collector, e.cb, e.attestation)
	t.Cleanup(e.shutdown)
	return e
}

func fallbackTestRequest(t *testing.T, model *Model, body string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://router.invalid/v1/responses?fixture=1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer synthetic-test-key")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	req = req.WithContext(model.WithBadRequestFallback(req.Context(), nil, func(accepted *Enclave) {
		recorder.Header().Set("Tinfoil-Enclave", accepted.host)
	}))
	return req, recorder
}

func TestBadRequestFallbackRecoversInference(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var rejected, accepted atomic.Int64
			first := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				rejected.Add(1)
				w.Header().Set("X-Rejected-Only", "private")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, fallbackTestError)
			})
			want := `{"output":"OK"}`
			if streaming {
				want = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
			}
			body := strings.Replace(fallbackTestBody, `"stream":true`, fmt.Sprintf(`"stream":%t`, streaming), 1)
			second := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				accepted.Add(1)
				got, _ := io.ReadAll(r.Body)
				if string(got) != body || r.URL.RawQuery != "fixture=1" || r.Header.Get("Authorization") != "Bearer synthetic-test-key" || r.Host == "router.invalid" {
					t.Errorf("retry changed the request: %s %s %s", got, r.URL, r.Host)
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				io.WriteString(w, want)
			})
			model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
			req, recorder := fallbackTestRequest(t, model, body)
			first.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK || recorder.Body.String() != want || recorder.Header().Get("Tinfoil-Enclave") != second.host || recorder.Header().Get("X-Rejected-Only") != "" {
				t.Fatalf("fallback response: %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			if rejected.Load() != 1 || accepted.Load() != 1 || first.cb.State() != cbOpen || !first.cb.quarantined.Load() {
				t.Fatalf("calls=%d/%d breaker=%v", rejected.Load(), accepted.Load(), first.cb.State())
			}
			if first.inflight.Load() != 0 || second.inflight.Load() != 0 {
				t.Fatal("completed fallback leaked in-flight accounting")
			}
			if got := testutil.ToFloat64(ProxySuccessTotal.WithLabelValues(first.modelName, first.host)); got != 0 {
				t.Fatalf("400 counted as %v successes", got)
			}
			if got := testutil.ToFloat64(ProxySuccessTotal.WithLabelValues(second.modelName, second.host)); got != 1 {
				t.Fatalf("fallback success counted %v times", got)
			}
			if got := testutil.ToFloat64(ProxyFailureTotal.WithLabelValues(first.modelName, first.host, "http_400")); got != 1 {
				t.Fatalf("400 failure count = %v", got)
			}
			selected, _, _, _, _ := model.SelectServing([]string{first.host, second.host}, nil, nil)
			if selected != second {
				t.Fatal("incompatible backend remained routable")
			}
		})
	}
}

func TestBadRequestFallbackPreservesUnconfirmedRejection(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTemporaryRedirect, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int64
			first := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "11")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, fallbackTestError)
			})
			second := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Location", "https://untrusted.invalid/")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"alternate rejection"}}`)
			})
			third := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) { t.Error("retried more than once") })
			model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second, third.host: third}}
			req, recorder := fallbackTestRequest(t, model, fallbackTestBody)
			req = req.WithContext(model.WithBadRequestFallback(req.Context(), []string{second.host, third.host}, func(*Enclave) { t.Error("rejected fallback reported accepted") }))
			first.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Per-request options disabled") || recorder.Header().Get("Retry-After") != "11" || attempts.Load() != 1 {
				t.Fatalf("original rejection lost: %d %v %s, attempts=%d", recorder.Code, recorder.Header(), recorder.Body.String(), attempts.Load())
			}
			if !first.cb.Closed() || first.cb.quarantined.Load() || second.cb.quarantined.Load() || second.inflight.Load() != 0 {
				t.Fatal("unconfirmed rejection quarantined a backend or leaked a response")
			}
		})
	}
}

func TestBadRequestFallbackDoesNotReplayUnsafeRequests(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
		replay           bool
	}{
		{"stateful_response", "/v1/responses", `{"previous_response_id":"other-replica-id"}`, http.StatusBadRequest, true},
		{"stateful_conversation", "/v1/responses", `{"conversation":"other-replica-id"}`, http.StatusBadRequest, true},
		{"trailing_json", "/v1/responses", `{} {}`, http.StatusBadRequest, true},
		{"tool_execution", "/execute", `{}`, http.StatusBadRequest, true},
		{"nonreplayable", "/v1/responses", `{}`, http.StatusBadRequest, false},
		{"unauthorized", "/v1/responses", `{}`, http.StatusUnauthorized, true},
		{"successful_stream", "/v1/responses", `{}`, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, fallbackTestError)
			})
			second := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unsafe request replayed") })
			model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
			req, recorder := fallbackTestRequest(t, model, tc.body)
			req.URL.Path = tc.path
			if !tc.replay {
				req.GetBody = nil
			}
			first.ServeHTTP(recorder, req)
			if recorder.Code != tc.status || first.cb.quarantined.Load() {
				t.Fatalf("response altered: %d", recorder.Code)
			}
		})
	}
}

func TestBadRequestFallbackRespectsPoolAndHealth(t *testing.T) {
	model := newTestModel("failed", "reserved", "shared", "other-org", "expired", "open", "overloaded")
	model.applyReservations(fallbackTestModel, []config.ReservationConfig{
		{OrgIDs: []string{"caller"}, Enclaves: []string{"failed", "reserved"}},
		{OrgIDs: []string{"other"}, Enclaves: []string{"other-org"}},
	}, []string{"failed", "reserved", "shared", "other-org", "expired", "open", "overloaded"})
	model.Enclaves["expired"].attestation = newAttestationState(time.Now().Add(-time.Hour), time.Now(), "old")
	model.Enclaves["open"].cb.storeState(cbOpen)
	model.Enclaves["overloaded"].metrics.cfg = &config.OverloadConfig{MaxRequestsWaiting: 1}
	model.Enclaves["overloaded"].metrics.updateLatest(2, time.Now())
	model.Enclaves["overloaded"].metrics.evaluateThresholds(2)
	t.Cleanup(model.Enclaves["overloaded"].shutdown)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(WithCallerOrg(req.Context(), "caller"))
	order := []string{"other-org", "expired", "open", "overloaded", "shared", "reserved"}
	if got := model.badRequestAlternative(model.Enclaves["failed"], req, order); got == nil || got.host != "reserved" {
		t.Fatalf("did not prefer caller's reservation: %v", got)
	}
	model.Enclaves["reserved"].cb.Quarantine()
	if got := model.badRequestAlternative(model.Enclaves["failed"], req, order); got == nil || got.host != "shared" {
		t.Fatalf("did not use healthy shared spill: %v", got)
	}
	model.Enclaves["shared"].cb.Quarantine()
	if got := model.badRequestAlternative(model.Enclaves["failed"], req, order); got != nil {
		t.Fatalf("used an ineligible fallback: %s", got.host)
	}
}

func TestQuarantineSurvivesSuccessAndExcludesDegradedRouting(t *testing.T) {
	model := newTestModel("incompatible")
	e := model.Enclaves["incompatible"]
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() { e.cb.RecordSuccess(); e.cb.Quarantine(); e.cb.RecordSuccess() })
	}
	workers.Wait()
	e.cb.lastFailureNano.Store(time.Now().Add(-2 * cbCooldown).UnixNano())
	if e.cb.Closed() || e.cb.State() != cbOpen {
		t.Fatal("a late success reopened a quarantined replica")
	}
	if _, claimed := e.cb.ClaimProbe(); claimed {
		t.Fatal("ordinary traffic probed a quarantined replica")
	}
	if got, _ := model.NextEnclave(nil); got != nil {
		t.Fatal("degraded routing selected quarantine")
	}
	if got, _ := model.NextEnclavePreferring([]string{e.host}, nil); got != nil {
		t.Fatal("cache order selected quarantine")
	}
	if got := model.CacheRoutePool(); len(got.Candidates) != 0 {
		t.Fatal("cache fallback included quarantine")
	}
	if model.HasHealthyEnclave(nil, nil) {
		t.Fatal("auto routing included quarantine")
	}
	model.installEnclaveLocked(e.host, newTestEnclave(e.host))
	if got, _ := model.NextEnclave(nil); got == nil {
		t.Fatal("replacement instance was not eligible")
	}
}

func TestClientErrorsDoNotResetBreakerAndReleaseProbe(t *testing.T) {
	e := newTestEnclave("client-error")
	e.modelName = fallbackTestModel
	e.cb.RecordFailure()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusBadRequest, Request: req}
	recordHTTPOutcome(e.modelName, e.host, e.cb, resp)
	if e.cb.ConsecutiveFailures() != 1 || !e.cb.Closed() {
		t.Fatal("client rejection changed breaker history")
	}
	e.cb.storeState(cbOpen)
	e.cb.lastFailureNano.Store(time.Now().Add(-2 * cbCooldown).UnixNano())
	claim := e.claimProbe()
	if claim == nil {
		t.Fatal("probe was not claimed")
	}
	resp.Request = req.WithContext(WithProbeClaim(context.Background(), claim))
	recordHTTPOutcome(e.modelName, e.host, e.cb, resp)
	if e.cb.State() != cbOpen || e.cb.ConsecutiveFailures() != 1 {
		t.Fatal("client rejection stranded or reset recovery probe")
	}
}

func TestInternalBadRequestFallback(t *testing.T) {
	for _, parsed := range []bool{false, true} {
		t.Run(fmt.Sprintf("parsed=%t", parsed), func(t *testing.T) {
			var rejected, accepted atomic.Int64
			first := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				rejected.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, fallbackTestError)
			})
			second := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				accepted.Add(1)
				io.WriteString(w, "accepted internal request")
			})
			model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
			model.applyReservations(fallbackTestModel, []config.ReservationConfig{{OrgIDs: []string{"caller"}, Enclaves: []string{first.host}}}, []string{first.host, second.host})
			em := &EnclaveManager{models: &sync.Map{}}
			em.models.Store(fallbackTestModel, model)
			ctx := WithCallerOrg(context.Background(), "caller")
			var resp *http.Response
			var err error
			if parsed {
				resp, err = em.DoModelRequestJSON(ctx, fallbackTestModel, "/v1/responses", map[string]any{"input": "hi"}, nil)
			} else {
				resp, err = em.DoModelRequest(ctx, fallbackTestModel, "/v1/responses", []byte(`{"input":"hi"}`), nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if first.inflight.Load() != 0 || second.inflight.Load() != 1 {
				t.Fatal("response lifetime not accounted to alternate")
			}
			resp.Body.Close()
			if err != nil || string(body) != "accepted internal request" || resp.StatusCode != http.StatusOK || rejected.Load() != 1 || accepted.Load() != 1 || !first.cb.quarantined.Load() || second.inflight.Load() != 0 {
				t.Fatalf("internal retry: status=%d body=%q error=%v calls=%d/%d", resp.StatusCode, body, err, rejected.Load(), accepted.Load())
			}
			if got := testutil.ToFloat64(ProxySuccessTotal.WithLabelValues(second.modelName, second.host)); got != 1 {
				t.Fatalf("accepted request counted %v times", got)
			}
		})
	}
}

func TestBadRequestFallbackUsesPinnedTransport(t *testing.T) {
	first := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, fallbackTestError)
	})
	var alternateCalls atomic.Int64
	second := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) { alternateCalls.Add(1) })
	transport := newAttestedTransport(strings.Repeat("0", len(second.tlsKeyFP)), second.attestation)
	t.Cleanup(transport.CloseIdleConnections)
	second.proxy.Transport = transport
	model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
	req, recorder := fallbackTestRequest(t, model, fallbackTestBody)
	first.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || alternateCalls.Load() != 0 || first.cb.quarantined.Load() || second.cb.ConsecutiveFailures() != 1 || second.inflight.Load() != 0 {
		t.Fatalf("unverified fallback: status=%d calls=%d failures=%d", recorder.Code, alternateCalls.Load(), second.cb.ConsecutiveFailures())
	}
}

func TestBadRequestFallbackCancellation(t *testing.T) {
	first := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, fallbackTestError)
	})
	entered := make(chan struct{})
	release := make(chan struct{})
	second := fallbackTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})
	t.Cleanup(func() { close(release) })
	model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
	req, recorder := fallbackTestRequest(t, model, fallbackTestBody)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); first.ServeHTTP(recorder, req.WithContext(ctx)) }()
	waitRenewalSignal(t, entered)
	cancel()
	waitRenewalSignal(t, done)
	if first.cb.quarantined.Load() || second.cb.ConsecutiveFailures() != 0 || second.inflight.Load() != 0 {
		t.Fatal("canceled retry poisoned a breaker or leaked in-flight accounting")
	}
}

func TestQuarantineSurvivesAttestationRenewal(t *testing.T) {
	server, key := renewalServer(t, nil)
	now := time.Now()
	em, model, existing := renewalManager(t, server, now.Add(attestationRenewalWindow/2), now.Add(-attestationRefreshInterval), func(string, string) (*verify.Verification, error) {
		return renewalVerification(key, now.Add(time.Hour)), nil
	})
	existing.cb.Quarantine()
	if err := em.addEnclave(renewalTestModel, existing.host); err != nil {
		t.Fatal(err)
	}
	if model.Enclaves[existing.host] != existing || !existing.cb.quarantined.Load() {
		t.Fatal("freshness renewal cleared compatibility quarantine")
	}
}
