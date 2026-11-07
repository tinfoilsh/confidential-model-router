package manager

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const retryTestBody = `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hello"}]}`

func retryTestEnclave(t *testing.T, handler http.HandlerFunc) *Enclave {
	t.Helper()
	server, _ := renewalServer(t, handler)
	_, _, enclave := renewalManager(t, server, time.Now().Add(time.Hour), time.Now(), nil)
	return enclave
}

func retryTestRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), renewalTestTimeout)
	t.Cleanup(cancel)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://router.example"+path, strings.NewReader(retryTestBody))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer tk_retry_test")
	r.Header.Set(UsageMetricsRequestHeader, "true")
	return r
}

func TestReplicaRetry(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		stream     bool
		second     int
	}{
		{"chat", "/v1/chat/completions", false, http.StatusOK},
		{"responses", "/v1/responses", false, http.StatusOK},
		{"stream", "/v1/completions", true, http.StatusOK},
		{"both fail", "/v1/chat/completions", false, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			first := retryTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				firstCalls.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", "30")
				w.Header().Set("X-Failed-Backend", "true")
				http.Error(w, "engine unavailable", http.StatusBadGateway)
			})
			second := retryTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				secondCalls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != retryTestBody || r.Header.Get("Authorization") != "Bearer tk_retry_test" || r.URL.Path != tc.path {
					t.Errorf("replay changed: body=%q path=%s err=%v", body, r.URL.Path, err)
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.second)
				if tc.stream {
					io.WriteString(w, "data: {\"choices\":[{\"text\":\"hello\"}]}\n\n")
					w.(http.Flusher).Flush()
					io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\ndata: [DONE]\n\n")
				} else {
					io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
				}
			})
			model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
			r := retryTestRequest(t, tc.path)
			if tc.stream {
				r.Header.Set("X-Tinfoil-Client-Requested-Usage", "true")
			}
			claim := &ProbeClaim{cb: first.cb}
			r = r.WithContext(WithProbeClaim(r.Context(), claim))
			deadline, _ := r.Context().Deadline()
			var selections int
			recorder := httptest.NewRecorder()
			first.ServeHTTPWithFailover(recorder, r, func(replay *http.Request) *Enclave {
				selections++
				if got, _ := replay.Context().Deadline(); !got.Equal(deadline) || ProbeClaimFromContext(replay.Context()) != nil {
					t.Fatal("retry lost its deadline or retained the first probe claim")
				}
				return model.SelectRetry(first.host, nil, nil, nil, nil)
			})
			if recorder.Code != tc.second || firstCalls.Load() != 1 || secondCalls.Load() != 1 || selections != 1 {
				t.Fatalf("status=%d first=%d second=%d selections=%d", recorder.Code, firstCalls.Load(), secondCalls.Load(), selections)
			}
			if first.cb.ConsecutiveFailures() != 1 || testutil.ToFloat64(ProxyFailureTotal.WithLabelValues(first.modelName, first.host, "http_502")) != 1 {
				t.Fatal("first failure was lost or double counted")
			}
			if second.inflight.Load() != 0 || testutil.ToFloat64(BackendInflight.WithLabelValues(second.modelName, second.host)) != 0 {
				t.Fatal("retry leaked dispatch reservation")
			}
			if recorder.Header().Get("Tinfoil-Enclave") != second.host || recorder.Header().Get("Retry-After") != "" || recorder.Header().Get("X-Failed-Backend") != "" {
				t.Fatalf("incorrect final headers: %v", recorder.Header())
			}
			if tc.second == http.StatusOK {
				if !strings.Contains(recorder.Header().Get(UsageMetricsResponseHeader), "completion=1") || second.cb.ConsecutiveFailures() != 0 {
					t.Fatal("successful retry lost usage or failed its breaker")
				}
				if tc.stream && (!recorder.Flushed || !strings.Contains(recorder.Body.String(), "hello")) {
					t.Fatal("retry did not stream the response")
				}
			} else if second.cb.ConsecutiveFailures() != 1 {
				t.Fatal("second failure did not reach its breaker")
			}
		})
	}
}

func TestReplicaRetryRestrictions(t *testing.T) {
	for _, scenario := range []string{"no alternate", "unbuffered", "body error", "canceled", "GET", "other endpoint", "HTTP 400", "HTTP 500", "HTTP 503", "informational headers", "transport error"} {
		t.Run(scenario, func(t *testing.T) {
			status := http.StatusBadGateway
			switch scenario {
			case "HTTP 400":
				status = http.StatusBadRequest
			case "HTTP 500":
				status = http.StatusInternalServerError
			case "HTTP 503":
				status = http.StatusServiceUnavailable
			}
			enclave := retryTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if scenario == "informational headers" {
					w.WriteHeader(http.StatusEarlyHints)
				}
				http.Error(w, "original failure", status)
			})
			r := retryTestRequest(t, "/v1/chat/completions")
			switch scenario {
			case "unbuffered":
				r.GetBody = nil
			case "body error":
				r.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("body unavailable") }
			case "GET":
				r.Method = http.MethodGet
			case "other endpoint":
				r.URL.Path = "/v1/audio/speech"
			case "canceled":
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			case "transport error":
				enclave.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					r.Body.Close()
					return nil, io.ErrUnexpectedEOF
				})
			}
			selections := 0
			recorder := httptest.NewRecorder()
			enclave.ServeHTTPWithFailover(recorder, r, func(*http.Request) *Enclave {
				selections++
				return nil
			})
			wantSelections := 0
			if scenario == "no alternate" {
				wantSelections = 1
			}
			if selections != wantSelections {
				t.Fatalf("unexpected retry selection: %d", selections)
			}
			if scenario != "informational headers" && recorder.Code != status {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, status, recorder.Body.String())
			}
		})
	}
}

func TestReplicaRetryDoesNotReplayStartedStream(t *testing.T) {
	enclave := retryTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"text\":\"partial\"}]}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	recorder := httptest.NewRecorder()
	aborted := false
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered != http.ErrAbortHandler {
					panic(recovered)
				}
				aborted = true
			}
		}()
		r := retryTestRequest(t, "/v1/completions")
		r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{}))
		enclave.ServeHTTPWithFailover(recorder, r, func(*http.Request) *Enclave {
			t.Fatal("started stream was replayed")
			return nil
		})
	}()
	if !aborted || recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "partial") {
		t.Fatalf("aborted=%v status=%d body=%s", aborted, recorder.Code, recorder.Body.String())
	}
}

func TestSelectRetryEligibility(t *testing.T) {
	for _, scenario := range []string{"reserved", "shared", "expired", "open", "half open", "overloaded", "stale", "missing metrics", "removed"} {
		t.Run(scenario, func(t *testing.T) {
			model := newTieredTestModel()
			alternate := model.Enclaves["sibling"]
			want := "sibling"
			if scenario != "reserved" {
				want = "shared"
			}
			switch scenario {
			case "shared", "removed":
				delete(model.Enclaves, alternate.host)
			case "expired":
				alternate.attestation = newAttestationState(time.Now().Add(-time.Second), time.Now(), "v1")
			case "open", "half open":
				tripBreaker(alternate)
				alternate.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown).UnixNano())
				if scenario == "half open" {
					alternate.claimProbe()
				}
			case "overloaded":
				observeCounts(alternate.metrics, reservedQueueLimit, 0)
			case "stale":
				counts, _ := alternate.metrics.latestSample()
				alternate.metrics.updateLatest(counts, time.Now().Add(-sampleStalenessLimit-time.Second))
			case "missing metrics":
				alternate.metrics = nil
			}
			primary, spill, policy := model.ReservationPools(reservationOverloadOrg)
			order := []string{"other-reserved", "warm", "sibling", "shared"}
			selected := model.SelectRetry("warm", order, primary, spill, policy)
			if selected == nil || selected.host != want || selected.inflight.Load() != 1 {
				t.Fatalf("selected=%v want=%s", selected, want)
			}
			selected.ReleaseDispatch()
			delete(model.Enclaves, "shared")
			if scenario != "reserved" {
				if selected := model.SelectRetry("warm", order, primary, spill, policy); selected != nil {
					selected.ReleaseDispatch()
					t.Fatalf("retried an ineligible alternate: %s", selected)
				}
			}
			delete(model.Enclaves, "sibling")
			if selected := model.SelectRetry("warm", order, primary, spill, policy); selected != nil {
				selected.ReleaseDispatch()
				t.Fatalf("escaped pool or retried failed host: %s", selected)
			}
		})
	}
}

func TestReplicaRetryKeepsAttestationEnforcement(t *testing.T) {
	first := retryTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Error(w, "engine unavailable", http.StatusBadGateway)
	})
	var calls atomic.Int32
	second := retryTestEnclave(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	second.proxy = newProxy(second.host, strings.Repeat("0", len(second.tlsKeyFP)), second.modelName, nil, second.cb, second.attestation)
	model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
	recorder := httptest.NewRecorder()
	first.ServeHTTPWithFailover(recorder, retryTestRequest(t, "/v1/chat/completions"), func(*http.Request) *Enclave {
		return model.SelectRetry(first.host, nil, nil, nil, nil)
	})
	if recorder.Code != http.StatusBadGateway || calls.Load() != 0 || second.cb.ConsecutiveFailures() != 1 || second.inflight.Load() != 0 {
		t.Fatalf("status=%d upstream=%d failures=%d inflight=%d", recorder.Code, calls.Load(), second.cb.ConsecutiveFailures(), second.inflight.Load())
	}
}
