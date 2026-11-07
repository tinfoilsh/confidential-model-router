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

func TestReplicaRetryStopsAfterSecondFailure(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			fail := func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				http.Error(w, "engine unavailable", http.StatusBadGateway)
			}
			first, second := retryTestEnclave(t, fail), retryTestEnclave(t, fail)
			model := &Model{Enclaves: map[string]*Enclave{first.host: first, second.host: second}}
			r := retryTestRequest(t, path)
			r = r.WithContext(WithProbeClaim(r.Context(), &ProbeClaim{cb: first.cb}))
			deadline, _ := r.Context().Deadline()
			selections := 0
			recorder := httptest.NewRecorder()
			first.ServeHTTPWithFailover(recorder, r, func(replay *http.Request) *Enclave {
				selections++
				if got, _ := replay.Context().Deadline(); !got.Equal(deadline) || ProbeClaimFromContext(replay.Context()) != nil {
					t.Fatal("retry lost its deadline or retained the first probe claim")
				}
				return model.SelectRetry(first.host, nil, nil, nil, nil)
			})
			if recorder.Code != http.StatusBadGateway || calls.Load() != 2 || selections != 1 {
				t.Fatalf("status=%d calls=%d selections=%d", recorder.Code, calls.Load(), selections)
			}
			if first.cb.ConsecutiveFailures() != 1 || second.cb.ConsecutiveFailures() != 1 || second.inflight.Load() != 0 {
				t.Fatal("retry lost a breaker failure or leaked its dispatch reservation")
			}
		})
	}
}

func TestReplicaRetryRestrictions(t *testing.T) {
	for _, scenario := range []string{"no alternate", "unbuffered", "body error", "canceled", "GET", "other endpoint", "HTTP 400", "HTTP 500", "HTTP 503", "transport error"} {
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
			if recorder.Code != status {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, status, recorder.Body.String())
			}
		})
	}
}

func TestReplicaRetryDoesNotReplayStartedResponse(t *testing.T) {
	for _, interim := range []bool{false, true} {
		enclave := retryTestEnclave(t, func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			if interim {
				w.WriteHeader(http.StatusEarlyHints)
				http.Error(w, "engine unavailable", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: partial\n\n")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		})
		router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(retryTestBody)), nil }
			enclave.ServeHTTPWithFailover(w, r, func(*http.Request) *Enclave {
				t.Error("started response was replayed")
				return nil
			})
		}))
		t.Cleanup(router.Close)
		client := router.Client()
		client.Timeout = renewalTestTimeout
		response, err := client.Post(router.URL+"/v1/completions", "application/json", strings.NewReader(retryTestBody))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if interim {
			if response.StatusCode != http.StatusBadGateway || err != nil {
				t.Fatalf("interim response: status=%d error=%v", response.StatusCode, err)
			}
		} else if response.StatusCode != http.StatusOK || err == nil || !strings.Contains(string(body), "partial") {
			t.Fatalf("broken stream: status=%d body=%q error=%v", response.StatusCode, body, err)
		}
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
