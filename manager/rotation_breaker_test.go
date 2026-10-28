package manager

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go/verify"
)

func TestRotationRecoveredOutcomesUseServingBreaker(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		status     int
		failure    bool
		disconnect bool
	}{
		{name: "HTTP failure", status: http.StatusInternalServerError, failure: true},
		{name: "transport failure", status: http.StatusBadGateway, failure: true, disconnect: true},
		{name: "success", status: http.StatusOK},
		{name: "client error", status: http.StatusBadRequest},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var requests, verifications atomic.Int32
			_, model, previous, _ := rotationFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != rotationBody {
					t.Errorf("body=%q err=%v", body, err)
				}
				if scenario.disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.WriteHeader(scenario.status)
				io.WriteString(w, "synthetic candidate response")
			}, func(key string) (*verify.Verification, error) {
				verifications.Add(1)
				return renewalVerification(key, time.Now().Add(time.Hour)), nil
			})
			previous.cb.RecordFailure()
			oldFailures := previous.cb.ConsecutiveFailures()
			selected := make([]*Enclave, cbFailureThreshold)
			for i := range selected {
				selected[i], _, _, _, _ = model.SelectServing(nil, false, nil, nil, nil)
				if selected[i] != previous {
					t.Fatal("expected admission on the old deployment")
				}
				defer selected[i].ReleaseDispatch()
			}
			healthy := newTestEnclave("healthy.example")
			healthy.modelName = renewalTestModel
			model.Enclaves[healthy.host] = healthy
			var dispatched atomic.Int32
			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				r.Body.Close()
				if err != nil {
					t.Error(err)
					return
				}
				r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
				r.Body, _ = r.GetBody()
				selected[dispatched.Add(1)-1].ServeHTTP(w, r)
			}))
			t.Cleanup(router.Close)
			client := router.Client()
			client.Timeout = renewalTestTimeout
			for i := range selected {
				response, err := client.Post(router.URL+"/v1/responses", "application/json", strings.NewReader(rotationBody))
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != scenario.status || readErr != nil {
					t.Fatalf("status=%d, want %d, read error=%v", response.StatusCode, scenario.status, readErr)
				}
				current := model.Enclaves[previous.host]
				if current == previous || !previous.cb.Retired() {
					t.Fatal("replacement was not installed")
				}
				wantFailures := int64(0)
				if scenario.failure {
					wantFailures = int64(i + 1)
				}
				if got := current.cb.ConsecutiveFailures(); got != wantFailures {
					t.Errorf("candidate failures=%d, want %d", got, wantFailures)
				}
				if got := previous.cb.ConsecutiveFailures(); got != oldFailures {
					t.Errorf("old deployment failures=%d, want unchanged %d", got, oldFailures)
				}
			}
			if requests.Load() != cbFailureThreshold || verifications.Load() != 1 {
				t.Fatalf("backend requests=%d verifications=%d", requests.Load(), verifications.Load())
			}
			next, _, _, _, _ := model.SelectServing([]string{previous.host}, true, nil, nil, nil)
			if next != nil {
				defer next.ReleaseDispatch()
			}
			if scenario.failure && next != healthy {
				t.Fatal("routing did not avoid the unhealthy candidate in favor of the healthy replica")
			}
			if !scenario.failure && next != model.Enclaves[previous.host] {
				t.Fatal("healthy candidate was not eligible for routing")
			}
		})
	}
}

func TestRotationOldDeploymentOutcomeDoesNotAffectReplacement(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	server, key := renewalServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		http.Error(w, "synthetic old deployment failure", http.StatusInternalServerError)
	})
	now := time.Now()
	em, model, previous := renewalManager(t, server, now.Add(time.Hour), now, func(string, string) (*verify.Verification, error) {
		return renewalVerification(strings.Repeat("0", len(key)), now.Add(time.Hour)), nil
	})
	t.Cleanup(func() {
		for _, current := range model.Enclaves {
			current.shutdown()
		}
	})
	defer unblock()
	previous.cb.RecordFailure()
	oldFailures := previous.cb.ConsecutiveFailures()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		previous.ServeHTTP(response, rotationRequest(t, previous.host))
	}()
	waitRenewalSignal(t, started)
	if err := em.refreshEnclave(renewalTestModel, previous.host); err != nil {
		t.Fatal(err)
	}
	current := model.Enclaves[previous.host]
	if current == previous || current.cb.ConsecutiveFailures() != 0 || !current.cb.Closed() {
		t.Fatal("replacement did not start with a fresh breaker")
	}
	unblock()
	waitRenewalSignal(t, done)
	if response.Code != http.StatusInternalServerError || previous.cb.ConsecutiveFailures() != oldFailures+1 {
		t.Fatalf("status=%d old failures=%d", response.Code, previous.cb.ConsecutiveFailures())
	}
	if current.cb.ConsecutiveFailures() != 0 || !current.cb.Closed() {
		t.Fatal("old deployment failure affected the replacement")
	}
}

func TestRotationRecoveredCancellationDoesNotFailCandidate(t *testing.T) {
	started := make(chan struct{})
	_, model, previous, _ := rotationFixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}, func(key string) (*verify.Verification, error) {
		return renewalVerification(key, time.Now().Add(time.Hour)), nil
	})
	tripBreaker(previous)
	previous.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown).UnixNano())
	selected, claim, _, _, _ := model.SelectServing(nil, true, nil, nil, nil)
	if selected != previous || claim == nil {
		t.Fatal("expected a recovery probe on the old deployment")
	}
	defer selected.ReleaseDispatch()
	req := rotationRequest(t, previous.host)
	ctx, cancel := context.WithCancel(WithProbeClaim(req.Context(), claim))
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		selected.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	}()
	waitRenewalSignal(t, started)
	cancel()
	waitRenewalSignal(t, done)
	current := model.Enclaves[previous.host]
	if current == previous || current.cb.ConsecutiveFailures() != 0 || !current.cb.Closed() {
		t.Fatal("cancellation penalized the candidate")
	}
	if previous.cb.ConsecutiveFailures() != cbFailureThreshold || previous.cb.State() != cbOpen {
		t.Fatal("old probe was not released without an additional failure")
	}
}
