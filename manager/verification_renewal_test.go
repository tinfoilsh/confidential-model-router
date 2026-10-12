package manager

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/tinfoil-go/document"
	"github.com/tinfoilsh/tinfoil-go/enclave"
	"github.com/tinfoilsh/tinfoil-go/verify"
	"github.com/tinfoilsh/tinfoil-go/verify/measurement"
)

const (
	renewalTestModel      = "gpt-oss-120b"
	renewalTestRepo       = "tinfoilsh/confidential-gpt-oss-120b"
	renewalTestDocument   = `{"format":"https://tinfoil.sh/predicate/attestation/v3"}`
	renewalTestResponse   = "verified response"
	renewalTestTimeout    = 5 * time.Second
	renewalTestQueueLimit = 8
)

type renewalVerifierFunc func([]byte, []byte, string) (*verify.Verification, error)

func (f renewalVerifierFunc) VerifyV3(body, nonce []byte, repo string) (*verify.Verification, error) {
	return f(body, nonce, repo)
}

func renewalVerification(key string, expiresAt time.Time) *verify.Verification {
	return &verify.Verification{
		CodeTag:            "v1.0.1",
		CodeMeasurement:    &measurement.Measurement{Type: measurement.SevGuestV2},
		EnclaveMeasurement: &measurement.Measurement{Type: measurement.SevGuestV2},
		CryptoMaterial: []document.CryptoMaterialItem{
			{ID: document.CryptoMaterialIDTLS, Format: document.KeySPKIFPSHA256V1Format, Data: key},
		},
		FreshnessExpiresAt: expiresAt,
		Metadata:           verify.VerificationMetadata{VerifiedAt: time.Now()},
	}
}

func renewalManager(t *testing.T, server *httptest.Server, expiresAt, verifiedAt time.Time, verifier renewalVerifierFunc) (*EnclaveManager, *Model, *Enclave) {
	t.Helper()
	key, err := enclave.CertPubkeyFP(server.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(server.URL, "https://")
	existing := newTestEnclave(host)
	existing.modelName = renewalTestModel
	existing.tlsKeyFP = key
	existing.predicate = measurement.SevGuestV2
	existing.attestation = newAttestationState(expiresAt, verifiedAt, "v1.0.0")
	existing.proxy = newProxy(host, key, renewalTestModel, nil, existing.cb, existing.attestation)
	t.Cleanup(existing.shutdown)
	model := &Model{
		Repo:            renewalTestRepo,
		Tag:             "v1.0.0",
		Enclaves:        map[string]*Enclave{host: existing},
		expectedHosts:   1,
		configuredHosts: []string{host},
	}
	em := &EnclaveManager{models: &sync.Map{}, verifier: verifier, refreshInterval: time.Minute}
	em.models.Store(renewalTestModel, model)
	return em, model, existing
}

func waitRenewalSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(renewalTestTimeout):
		t.Fatal("timed out waiting for renewal test request")
	}
}

func requireRenewalResponse(t *testing.T, transport http.RoundTripper, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), renewalTestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != renewalTestResponse {
		t.Fatalf("renewed request: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
}

func requireRenewalExpired(t *testing.T, transport http.RoundTripper, url string) {
	t.Helper()
	body := &trackedVerificationBody{Reader: bytes.NewReader([]byte("private request"))}
	request, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, errAttestationExpired) || !body.closed {
		t.Fatalf("expired request: error=%v body closed=%v", err, body.closed)
	}
}

func TestAttestationRenewalPreservesServingStateAndHeldClients(t *testing.T) {
	heldStarted := make(chan struct{})
	heldRelease := make(chan struct{})
	releaseHeld := sync.OnceFunc(func() { close(heldRelease) })
	var applicationCalls atomic.Int32
	var challenge string
	var challengeMu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/tinfoil-attestation" {
			challengeMu.Lock()
			challenge = r.URL.Query().Get("nonce")
			challengeMu.Unlock()
			io.WriteString(w, renewalTestDocument)
			return
		}
		applicationCalls.Add(1)
		if r.URL.Path == "/held" {
			close(heldStarted)
			<-heldRelease
		}
		io.WriteString(w, renewalTestResponse)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(releaseHeld)
	trustVerificationServer(t, server)
	key, err := enclave.CertPubkeyFP(server.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	oldVerifiedAt := now.Add(-attestationRefreshInterval)
	newExpiry := now.Add(2 * attestationRenewalWindow)
	var verificationCalls int
	em, model, existing := renewalManager(t, server, now.Add(time.Hour), oldVerifiedAt, func(body, nonce []byte, repo string) (*verify.Verification, error) {
		verificationCalls++
		challengeMu.Lock()
		gotChallenge := challenge
		challengeMu.Unlock()
		if string(body) != renewalTestDocument || len(nonce) == 0 || hex.EncodeToString(nonce) != gotChallenge || repo != renewalTestRepo {
			t.Errorf("incorrect verification inputs: document=%q nonce=%x challenge=%q repo=%q", body, nonce, gotChallenge, repo)
		}
		return renewalVerification(key, newExpiry), nil
	})
	oldState, oldProxy, oldBreaker, oldMetrics := existing.attestation, existing.proxy, existing.cb, existing.metrics
	heldTransport := newAttestedTransport(key, oldState)
	t.Cleanup(heldTransport.CloseIdleConnections)

	pollStarted := make(chan struct{})
	pollCanceled := make(chan struct{})
	var pollStartOnce, pollCancelOnce sync.Once
	existing.metrics.client = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		pollStartOnce.Do(func() { close(pollStarted) })
		<-request.Context().Done()
		pollCancelOnce.Do(func() { close(pollCanceled) })
		return nil, request.Context().Err()
	})}
	model.Overload = &config.OverloadConfig{MaxRequestsWaiting: renewalTestQueueLimit}
	existing.metrics.setConfig(model.Overload)
	waitRenewalSignal(t, pollStarted)

	heldResponse := httptest.NewRecorder()
	heldDone := make(chan struct{})
	go func() {
		defer close(heldDone)
		existing.ServeHTTP(heldResponse, httptest.NewRequest(http.MethodGet, "https://router.example/held", nil))
	}()
	waitRenewalSignal(t, heldStarted)
	tripBreaker(existing)
	existing.cb.lastFailureNano.Store(now.Add(-cbCooldown - time.Second).UnixNano())
	claim := existing.claimProbe()
	if claim == nil {
		t.Fatal("expected a claimed recovery probe before renewal")
	}

	oldDeadline := now.Add(-time.Second)
	oldState.current.Store(&verifiedAttestation{expiresAt: oldDeadline, verifiedAt: oldVerifiedAt, codeTag: "v1.0.0"})
	requireRenewalExpired(t, heldTransport, server.URL+"/response")
	if err := em.addEnclave(renewalTestModel, existing.host); err != nil {
		t.Fatal(err)
	}
	current := model.Enclaves[existing.host]
	if current != existing || current.attestation != oldState || current.proxy != oldProxy || current.cb != oldBreaker || current.metrics != oldMetrics {
		t.Fatal("same-key renewal replaced serving state")
	}
	if current.inflight.Load() != 1 || current.cb.State() != cbHalfOpen || current.cb.ConsecutiveFailures() != cbFailureThreshold || current.cb.Retired() {
		t.Fatalf("renewal lost active runtime state: inflight=%d breaker=%v failures=%d retired=%v", current.inflight.Load(), current.cb.State(), current.cb.ConsecutiveFailures(), current.cb.Retired())
	}
	select {
	case <-pollCanceled:
		t.Fatal("same-key renewal restarted the metrics poller")
	default:
	}
	updated := oldState.load()
	if verificationCalls != 1 || !updated.expiresAt.Equal(newExpiry) || !updated.verifiedAt.After(oldVerifiedAt) || updated.codeTag != "v1.0.1" {
		t.Fatalf("renewal did not publish verified evidence: calls=%d state=%+v", verificationCalls, updated)
	}
	requireRenewalResponse(t, heldTransport, server.URL+"/response")
	if applicationCalls.Load() != 2 {
		t.Fatalf("application calls=%d, want held request plus renewed request", applicationCalls.Load())
	}
	releaseHeld()
	waitRenewalSignal(t, heldDone)
	if heldResponse.Code != http.StatusOK || heldResponse.Body.String() != renewalTestResponse || current.inflight.Load() != 0 || !current.cb.Closed() {
		t.Fatalf("held request did not finish against preserved state: status=%d body=%q inflight=%d breaker=%v", heldResponse.Code, heldResponse.Body.String(), current.inflight.Load(), current.cb.State())
	}
}

func TestAttestationRenewalFailurePreservesEvidence(t *testing.T) {
	for _, failure := range []string{"fetch", "verification"} {
		t.Run(failure, func(t *testing.T) {
			var applicationCalls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/.well-known/tinfoil-attestation" {
					if failure == "fetch" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					io.WriteString(w, renewalTestDocument)
					return
				}
				applicationCalls.Add(1)
				io.WriteString(w, renewalTestResponse)
			}))
			t.Cleanup(server.Close)
			trustVerificationServer(t, server)
			now := time.Now()
			var verificationCalls int
			em, model, existing := renewalManager(t, server, now.Add(attestationRenewalWindow/2), now.Add(-time.Minute), func([]byte, []byte, string) (*verify.Verification, error) {
				verificationCalls++
				return nil, errors.New("freshness witness unavailable")
			})
			before := existing.attestation.load()
			transport := newAttestedTransport(existing.tlsKeyFP, existing.attestation)
			t.Cleanup(transport.CloseIdleConnections)
			if err := em.addEnclave(renewalTestModel, existing.host); err == nil {
				t.Fatal("failed renewal was accepted")
			}
			if model.Enclaves[existing.host] != existing || existing.attestation.load() != before {
				t.Fatal("failed renewal replaced cached evidence or advanced its verification time")
			}
			wantVerificationCalls := 1
			if failure == "fetch" {
				wantVerificationCalls = 0
			}
			if verificationCalls != wantVerificationCalls {
				t.Fatalf("verification calls=%d, want %d", verificationCalls, wantVerificationCalls)
			}
			requireRenewalResponse(t, transport, server.URL+"/response")
			existing.attestation.current.Store(&verifiedAttestation{expiresAt: now.Add(-time.Second), verifiedAt: before.verifiedAt, codeTag: before.codeTag})
			requireRenewalExpired(t, transport, server.URL+"/response")
			if applicationCalls.Load() != 1 {
				t.Fatalf("expired evidence reached backend: application calls=%d", applicationCalls.Load())
			}
		})
	}
}

func TestAttestationKeyChangeDoesNotRenewHeldClients(t *testing.T) {
	for _, changedKey := range []string{"tls", "hpke"} {
		t.Run(changedKey, func(t *testing.T) {
			var applicationCalls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/.well-known/tinfoil-attestation" {
					io.WriteString(w, renewalTestDocument)
					return
				}
				applicationCalls.Add(1)
				io.WriteString(w, renewalTestResponse)
			}))
			t.Cleanup(server.Close)
			trustVerificationServer(t, server)
			key, err := enclave.CertPubkeyFP(server.Certificate())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			newExpiry := now.Add(2 * attestationRenewalWindow)
			em, model, previous := renewalManager(t, server, now.Add(-time.Second), now.Add(-attestationRefreshInterval), func([]byte, []byte, string) (*verify.Verification, error) {
				return renewalVerification(key, newExpiry), nil
			})
			if changedKey == "tls" {
				previous.tlsKeyFP = strings.Repeat("0", len(key))
			} else {
				previous.hpkeKey = strings.Repeat("0", len(key))
			}
			previousState := previous.attestation
			heldTransport := newAttestedTransport(previous.tlsKeyFP, previousState)
			t.Cleanup(heldTransport.CloseIdleConnections)
			if err := em.addEnclave(renewalTestModel, previous.host); err != nil {
				t.Fatal(err)
			}
			current := model.Enclaves[previous.host]
			t.Cleanup(current.shutdown)
			if current == previous || current.attestation == previousState || current.cb == previous.cb || !previous.cb.Retired() {
				t.Fatal("changed key reused the retired backend's verification or runtime state")
			}
			if !current.attestation.load().expiresAt.Equal(newExpiry) || !previousState.load().expiresAt.Before(now) {
				t.Fatal("key change extended the old identity's approval")
			}
			requireRenewalExpired(t, heldTransport, server.URL+"/response")
			currentTransport := newAttestedTransport(current.tlsKeyFP, current.attestation)
			t.Cleanup(currentTransport.CloseIdleConnections)
			requireRenewalResponse(t, currentTransport, server.URL+"/response")
			if applicationCalls.Load() != 1 {
				t.Fatalf("retired client reached the new backend: application calls=%d", applicationCalls.Load())
			}
		})
	}
}

func TestAttestationPeriodicVerificationBeforeRenewalWindow(t *testing.T) {
	var challenges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenges.Add(1)
		io.WriteString(w, renewalTestDocument)
	}))
	t.Cleanup(server.Close)
	trustVerificationServer(t, server)
	key, err := enclave.CertPubkeyFP(server.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	expiresAt := now.Add(2 * attestationRenewalWindow)
	verifiedAt := now.Add(-attestationRefreshInterval - time.Second)
	var verificationCalls int
	em, _, existing := renewalManager(t, server, expiresAt, verifiedAt, func([]byte, []byte, string) (*verify.Verification, error) {
		verificationCalls++
		return renewalVerification(key, expiresAt), nil
	})
	if err := em.addEnclave(renewalTestModel, existing.host); err != nil {
		t.Fatal(err)
	}
	updated := existing.attestation.load()
	if challenges.Load() != 1 || verificationCalls != 1 || !updated.verifiedAt.After(verifiedAt) || !updated.expiresAt.Equal(expiresAt) {
		t.Fatalf("periodic verification: challenges=%d verifications=%d state=%+v", challenges.Load(), verificationCalls, updated)
	}
}

func TestAttestationRenewalDuringConfigurationOutage(t *testing.T) {
	var challenges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/tinfoil-attestation" {
			challenges.Add(1)
			io.WriteString(w, renewalTestDocument)
			return
		}
		io.WriteString(w, renewalTestResponse)
	}))
	t.Cleanup(server.Close)
	trustVerificationServer(t, server)
	key, err := enclave.CertPubkeyFP(server.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	verifiedAt := now.Add(-attestationRefreshInterval - time.Second)
	newExpiry := now.Add(2 * attestationRenewalWindow)
	em, model, existing := renewalManager(t, server, now.Add(time.Hour), verifiedAt, func([]byte, []byte, string) (*verify.Verification, error) {
		return renewalVerification(key, newExpiry), nil
	})
	em.updateConfigURL = t.TempDir() + "/missing-config.yaml"
	previousConfigUpdate := now.Add(-time.Minute)
	em.lastSuccessfulUpdate = previousConfigUpdate
	transport := newAttestedTransport(key, existing.attestation)
	t.Cleanup(transport.CloseIdleConnections)
	if err := em.sync(); err == nil || !strings.Contains(err.Error(), "failed to fetch config") {
		t.Fatalf("configuration outage was not reported: %v", err)
	}
	if !em.lastSuccessfulUpdate.Equal(previousConfigUpdate) {
		t.Fatal("verification renewal falsely advanced configuration success time")
	}
	updated := existing.attestation.load()
	if challenges.Load() != 1 || !updated.expiresAt.Equal(newExpiry) || !updated.verifiedAt.After(verifiedAt) {
		t.Fatalf("configuration outage prevented evidence renewal: challenges=%d state=%+v", challenges.Load(), updated)
	}
	if len(model.Enclaves) != 1 || model.Enclaves[existing.host] != existing || len(model.configuredHosts) != 1 || model.configuredHosts[0] != existing.host {
		t.Fatal("configuration outage changed the accepted backend set")
	}
	requireRenewalResponse(t, transport, server.URL+"/response")
}

func TestAttestationRejectsMissingOrExpiredVerificationDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresAt time.Time
	}{
		{name: "missing"},
		{name: "expired", expiresAt: time.Now().Add(-time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, renewalTestDocument)
			}))
			t.Cleanup(server.Close)
			trustVerificationServer(t, server)
			key, err := enclave.CertPubkeyFP(server.Certificate())
			if err != nil {
				t.Fatal(err)
			}
			var verificationCalls int
			em, model, previous := renewalManager(t, server, time.Now(), time.Now(), func([]byte, []byte, string) (*verify.Verification, error) {
				verificationCalls++
				return renewalVerification(key, tc.expiresAt), nil
			})
			delete(model.Enclaves, previous.host)
			if err := em.addEnclave(renewalTestModel, previous.host); err == nil {
				t.Fatal("accepted verification without a valid authenticated deadline")
			}
			if verificationCalls != 1 || len(model.Enclaves) != 0 || model.Tag != "v1.0.0" || model.SourceMeasurement != nil {
				t.Fatalf("invalid deadline modified model state: verification calls=%d enclaves=%d tag=%q measurement=%v", verificationCalls, len(model.Enclaves), model.Tag, model.SourceMeasurement)
			}
		})
	}
}
