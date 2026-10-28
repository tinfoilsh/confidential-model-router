package manager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go/enclave"
	"github.com/tinfoilsh/tinfoil-go/verify"
)

const (
	rotationRequests = 8
	rotationBody     = `{"model":"gpt-oss-120b","input":"private synthetic request","stream":true}`
	rotationStream   = "data: first\n\ndata: second\n\ndata: [DONE]\n\n"
)

func rotationFixture(t *testing.T, handler http.HandlerFunc, verifier func(string) (*verify.Verification, error)) (*EnclaveManager, *Model, *Enclave, *rotationTransport) {
	t.Helper()
	server, key := renewalServer(t, handler)
	now := time.Now()
	em, model, previous := renewalManager(t, server, now.Add(2*attestationRenewalWindow), now, func(string, string) (*verify.Verification, error) {
		return verifier(key)
	})
	previous.tlsKeyFP = strings.Repeat("0", len(key))
	previous.proxy = newProxy(previous.host, previous.tlsKeyFP, renewalTestModel, nil, previous.cb, previous.attestation)
	transport := &rotationTransport{base: previous.proxy.Transport, refresh: func(ctx context.Context) (http.RoundTripper, error) {
		return em.recoverEnclave(ctx, renewalTestModel, previous.host, previous.tlsKeyFP)
	}}
	previous.proxy.Transport = transport
	t.Cleanup(func() {
		for _, current := range model.Enclaves {
			current.shutdown()
		}
	})
	return em, model, previous, transport
}

func rotationRequest(t *testing.T, host string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://"+host+"/v1/responses", strings.NewReader(rotationBody))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRotationRecoveryVerifiesOnceAndReplaysUnsentRequests(t *testing.T) {
	var upstream, verifications atomic.Int32
	_, model, previous, transport := rotationFixture(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != rotationBody {
			t.Errorf("body=%q err=%v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, rotationStream)
	}, func(key string) (*verify.Verification, error) {
		verifications.Add(1)
		if upstream.Load() != 0 {
			t.Error("request reached backend before verification")
		}
		return renewalVerification(key, time.Now().Add(2*attestationRenewalWindow)), nil
	})
	var wg sync.WaitGroup
	for range rotationRequests {
		req := rotationRequest(t, previous.host)
		wg.Go(func() {
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Errorf("recovery: %v", err)
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != rotationStream {
				t.Errorf("response=%q status=%d err=%v", body, response.StatusCode, err)
			}
		})
	}
	wg.Wait()
	if upstream.Load() != rotationRequests || verifications.Load() != 1 {
		t.Fatalf("upstream=%d verifications=%d", upstream.Load(), verifications.Load())
	}
	if model.Enclaves[previous.host] == previous || !previous.cb.Retired() {
		t.Fatal("verified replacement was not installed")
	}
}

func TestRotationRecoveryFailsClosed(t *testing.T) {
	for _, scenario := range []string{"verification rejected", "expired evidence", "wrong verified key", "unchanged key", "removed during verification"} {
		t.Run(scenario, func(t *testing.T) {
			var upstream, verifications atomic.Int32
			var model *Model
			var previous *Enclave
			_, model, previous, transport := rotationFixture(t, func(w http.ResponseWriter, r *http.Request) { upstream.Add(1) }, func(key string) (*verify.Verification, error) {
				verifications.Add(1)
				switch scenario {
				case "verification rejected":
					return nil, errors.New("untrusted release")
				case "expired evidence":
					return renewalVerification(key, time.Now().Add(-time.Second)), nil
				case "wrong verified key":
					return renewalVerification(strings.Repeat("1", len(key)), time.Now().Add(time.Hour)), nil
				case "unchanged key":
					return renewalVerification(previous.tlsKeyFP, time.Now().Add(time.Hour)), nil
				case "removed during verification":
					model.mu.Lock()
					delete(model.configuredHosts, previous.host)
					delete(model.Enclaves, previous.host)
					model.mu.Unlock()
				}
				return renewalVerification(key, time.Now().Add(time.Hour)), nil
			})
			response, err := transport.RoundTrip(rotationRequest(t, previous.host))
			if response != nil {
				response.Body.Close()
			}
			if err == nil || upstream.Load() != 0 || verifications.Load() != 1 {
				t.Fatalf("err=%v upstream=%d verifications=%d", err, upstream.Load(), verifications.Load())
			}
			if scenario == "removed during verification" && len(model.Enclaves) != 0 {
				t.Fatal("recovery reintroduced a removed backend")
			}
		})
	}
}

func TestRotationRecoveryDoesNotReplayUnbufferedBody(t *testing.T) {
	var verifications atomic.Int32
	_, _, previous, transport := rotationFixture(t, nil, func(key string) (*verify.Verification, error) {
		verifications.Add(1)
		return renewalVerification(key, time.Now().Add(time.Hour)), nil
	})
	req := rotationRequest(t, previous.host)
	body := &trackedVerificationBody{Reader: bytes.NewReader([]byte(rotationBody))}
	req.Body = body
	req.GetBody = nil
	_, err := transport.RoundTrip(req)
	if err == nil || verifications.Load() != 0 || !body.closed {
		t.Fatalf("err=%v verifications=%d closed=%v", err, verifications.Load(), body.closed)
	}
}

func TestRotationRecoveryDoesNotReplayAfterHeadersOrHTTPError(t *testing.T) {
	for _, scenario := range []string{"headers sent", "HTTP 500", "ordinary transport failure", "expired attestation"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			transport := &rotationTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				switch scenario {
				case "headers sent":
					httptrace.ContextClientTrace(r.Context()).WroteHeaders()
					return nil, &enclave.AttestationError{Err: errors.New("late failure")}
				case "HTTP 500":
					return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("upstream failure"))}, nil
				case "expired attestation":
					return nil, &enclave.AttestationError{Err: errAttestationExpired}
				default:
					return nil, io.ErrUnexpectedEOF
				}
			}), refresh: func(context.Context) (http.RoundTripper, error) {
				t.Error("unexpected recovery")
				return nil, errors.New("unexpected recovery")
			}}
			response, err := transport.RoundTrip(rotationRequest(t, "backend.example"))
			if scenario == "HTTP 500" {
				if err != nil || response == nil || response.StatusCode != http.StatusInternalServerError {
					t.Fatalf("response=%v err=%v", response, err)
				}
				response.Body.Close()
			} else if err == nil {
				t.Fatal("failure was hidden")
			}
			if calls != 1 {
				t.Fatalf("attempts=%d", calls)
			}
		})
	}
}

func TestRotationRecoveryCallerCancellation(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	em, _, previous, transport := rotationFixture(t, nil, func(key string) (*verify.Verification, error) {
		close(started)
		<-release
		defer close(done)
		return nil, errors.New("verification unavailable")
	})
	t.Cleanup(func() {
		unblock()
		select {
		case <-started:
			<-done
		default:
		}
		em.verifications.Do(renewalTestModel+"\x00"+previous.host, func() (any, error) { return nil, nil })
	})
	req := rotationRequest(t, previous.host)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := transport.RoundTrip(req.WithContext(ctx)); result <- err }()
	waitRenewalSignal(t, started)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(renewalTestTimeout):
		t.Fatal("canceled request waited for verification")
	}
}

func TestRotationProxyReturnsRecoveredStream(t *testing.T) {
	_, _, previous, _ := rotationFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, rotationStream)
	}, func(key string) (*verify.Verification, error) {
		return renewalVerification(key, time.Now().Add(time.Hour)), nil
	})
	response := httptest.NewRecorder()
	previous.ServeHTTP(response, rotationRequest(t, previous.host))
	if response.Code != http.StatusOK || response.Body.String() != rotationStream {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestRotationRecoveryRejectsUnverifiedDocument(t *testing.T) {
	var inference, challenges atomic.Int32
	em, model, previous, transport := rotationFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			inference.Add(1)
		} else {
			challenges.Add(1)
		}
		io.WriteString(w, `{"format":"https://tinfoil.sh/predicate/attestation/v3"}`)
	}, nil)
	em.verifier = verifyEnclave
	response, err := transport.RoundTrip(rotationRequest(t, previous.host))
	if response != nil {
		response.Body.Close()
	}
	if err == nil || challenges.Load() == 0 || inference.Load() != 0 || model.Enclaves[previous.host] != previous {
		t.Fatalf("err=%v challenges=%d inference=%d replacement=%v", err, challenges.Load(), inference.Load(), model.Enclaves[previous.host] != previous)
	}
}
