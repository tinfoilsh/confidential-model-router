package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go/document"
	"github.com/tinfoilsh/tinfoil-go/enclave"
	"github.com/tinfoilsh/tinfoil-go/verify"
	"github.com/tinfoilsh/tinfoil-go/verify/measurement"
)

const (
	renewalTestModel    = "gpt-oss-120b"
	renewalTestRepo     = "tinfoilsh/confidential-gpt-oss-120b"
	renewalTestResponse = "verified response"
	renewalTestTimeout  = 5 * time.Second
	renewalTestRefresh  = time.Minute
)

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

func renewalServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, renewalTestResponse)
		}
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	trustVerificationServer(t, server)
	key, err := enclave.CertPubkeyFP(server.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	return server, key
}

func renewalManager(t *testing.T, server *httptest.Server, expiresAt, verifiedAt time.Time, verifier func(string, string) (*verify.Verification, error)) (*EnclaveManager, *Model, *Enclave) {
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
		configuredHosts: map[string]verificationAttempt{host: {}},
	}
	em := &EnclaveManager{models: &sync.Map{}, verifier: verifier, refreshInterval: renewalTestRefresh}
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
	server, key := renewalServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/held" {
			close(heldStarted)
			<-heldRelease
		}
		io.WriteString(w, renewalTestResponse)
	})
	t.Cleanup(releaseHeld)
	now := time.Now()
	oldVerifiedAt := now.Add(-attestationRefreshInterval)
	newExpiry := now.Add(2 * attestationRenewalWindow)
	em, model, existing := renewalManager(t, server, now.Add(time.Hour), oldVerifiedAt, func(host, repo string) (*verify.Verification, error) {
		if host != strings.TrimPrefix(server.URL, "https://") || repo != renewalTestRepo {
			t.Fatalf("incorrect verification target: host=%q repo=%q", host, repo)
		}
		return renewalVerification(key, newExpiry), nil
	})
	oldState, oldProxy, oldBreaker, oldMetrics := existing.attestation, existing.proxy, existing.cb, existing.metrics
	heldTransport := newAttestedTransport(key, oldState)
	t.Cleanup(heldTransport.CloseIdleConnections)

	heldResponse := httptest.NewRecorder()
	heldDone := make(chan struct{})
	reserveTestDispatch(t, existing)
	go func() {
		defer close(heldDone)
		defer existing.ReleaseDispatch()
		existing.ServeHTTP(heldResponse, httptest.NewRequest(http.MethodGet, "https://router.example/held", nil))
	}()
	waitRenewalSignal(t, heldStarted)
	tripBreaker(existing)
	existing.cb.lastFailureNano.Store(now.Add(-cbCooldown - time.Second).UnixNano())
	if existing.claimProbe() == nil {
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
	if current.inflight.Load() != 1 || current.cb.Retired() {
		t.Fatalf("renewal lost active runtime state: inflight=%d retired=%v", current.inflight.Load(), current.cb.Retired())
	}
	if current.cb.State() != cbHalfOpen || current.cb.ConsecutiveFailures() != cbFailureThreshold {
		t.Fatal("renewal reset the claimed recovery probe or its failure count")
	}
	updated := oldState.load()
	if !updated.expiresAt.Equal(newExpiry) || !updated.verifiedAt.After(oldVerifiedAt) || updated.codeTag != "v1.0.1" {
		t.Fatalf("renewal did not publish verified evidence: %+v", updated)
	}
	requireRenewalResponse(t, heldTransport, server.URL+"/response")
	releaseHeld()
	waitRenewalSignal(t, heldDone)
	if heldResponse.Code != http.StatusOK || heldResponse.Body.String() != renewalTestResponse || current.inflight.Load() != 0 || !current.cb.Closed() {
		t.Fatalf("held request did not finish against preserved state: status=%d body=%q inflight=%d breaker=%v", heldResponse.Code, heldResponse.Body.String(), current.inflight.Load(), current.cb.State())
	}
}

func TestAttestationRenewalFailurePreservesEvidence(t *testing.T) {
	server, _ := renewalServer(t, nil)
	now := time.Now()
	em, model, existing := renewalManager(t, server, now.Add(attestationRenewalWindow/2), now.Add(-time.Minute), func(string, string) (*verify.Verification, error) {
		return nil, errors.New("collateral unavailable")
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
	if attempt := model.configuredHosts[existing.host]; attempt.lastError != verificationVerify || attempt.failures != 1 {
		t.Fatalf("verification failure recorded as %+v", attempt)
	}
	requireRenewalResponse(t, transport, server.URL+"/response")
	existing.attestation.current.Store(&verifiedAttestation{expiresAt: now.Add(-time.Second), verifiedAt: before.verifiedAt, codeTag: before.codeTag})
	requireRenewalExpired(t, transport, server.URL+"/response")
}

func TestAttestationKeyChangeDoesNotRenewHeldClients(t *testing.T) {
	for _, changedKey := range []string{"tls", "hpke"} {
		t.Run(changedKey, func(t *testing.T) {
			server, key := renewalServer(t, nil)
			now := time.Now()
			newExpiry := now.Add(2 * attestationRenewalWindow)
			em, model, previous := renewalManager(t, server, now.Add(-time.Second), now.Add(-attestationRefreshInterval), func(string, string) (*verify.Verification, error) {
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
		})
	}
}

func TestAttestationPeriodicVerificationBeforeRenewalWindow(t *testing.T) {
	server, key := renewalServer(t, nil)
	now := time.Now()
	expiresAt := now.Add(2 * attestationRenewalWindow)
	verifiedAt := now.Add(-attestationRefreshInterval + renewalTestRefresh/2)
	em, _, existing := renewalManager(t, server, expiresAt, verifiedAt, func(string, string) (*verify.Verification, error) {
		return renewalVerification(key, expiresAt), nil
	})
	if err := em.addEnclave(renewalTestModel, existing.host); err != nil {
		t.Fatal(err)
	}
	updated := existing.attestation.load()
	if !updated.verifiedAt.After(verifiedAt) || !updated.expiresAt.Equal(expiresAt) {
		t.Fatalf("periodic verification did not update evidence: %+v", updated)
	}
}

func TestAttestationRenewalUsesAcceptedConfiguration(t *testing.T) {
	const configMode = 0o600
	server, key := renewalServer(t, nil)
	now := time.Now()
	verifiedAt := now.Add(-attestationRefreshInterval - time.Second)
	newExpiry := now.Add(time.Hour)
	calls := 0
	em, model, existing := renewalManager(t, server, now.Add(time.Minute), verifiedAt, func(host, repo string) (*verify.Verification, error) {
		if host != strings.TrimPrefix(server.URL, "https://") || repo != renewalTestRepo {
			t.Errorf("verification escaped accepted configuration: %s %s", host, repo)
		}
		calls++
		return renewalVerification(key, newExpiry), nil
	})
	model.configuredHosts[existing.host] = verificationAttempt{failures: 1}
	path := t.TempDir() + "/config.yaml"
	acceptedConfig := fmt.Sprintf("models:\n  %s:\n    repo: %s\n    enclaves: [%q]\n", renewalTestModel, renewalTestRepo, existing.host)
	transport := newAttestedTransport(key, existing.attestation)
	t.Cleanup(transport.CloseIdleConnections)
	for _, tc := range []struct {
		name, config string
		retained     bool
	}{
		{"accepted", acceptedConfig, true},
		{"unavailable", "", true},
		{"repository change", strings.NewReplacer(renewalTestRepo, "tinfoilsh/untrusted", existing.host, "untrusted.example").Replace(acceptedConfig), true},
		{"host removed", fmt.Sprintf("models:\n  %s:\n    repo: %s\n", renewalTestModel, renewalTestRepo), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.config), configMode); err != nil {
				t.Fatal(err)
			}
			em.updateConfigURL = path
			if tc.config == "" {
				em.updateConfigURL += ".missing"
			}
			beforeConfig, beforeRenewal, beforeCalls := em.lastSuccessfulUpdate, existing.attestation.load(), calls
			if err := em.sync(); (err != nil) != (tc.config == "") {
				t.Fatalf("unexpected config fetch result: %v", err)
			}
			if (tc.config == "") != em.lastSuccessfulUpdate.Equal(beforeConfig) {
				t.Fatal("configuration success timestamp does not match fetch result")
			}
			if !tc.retained {
				if calls != beforeCalls || len(model.configuredHosts) != 0 || len(model.Enclaves) != 0 || !existing.cb.Retired() {
					t.Fatal("removed backend was retained or renewed")
				}
				return
			}
			updated := existing.attestation.load()
			if calls != beforeCalls+1 || !updated.expiresAt.Equal(newExpiry) || !updated.verifiedAt.After(beforeRenewal.verifiedAt) {
				t.Fatal("accepted backend did not renew exactly once")
			}
			if len(model.Enclaves) != 1 || model.Enclaves[existing.host] != existing || len(model.configuredHosts) != 1 || model.configuredHosts[existing.host].failures != 1 {
				t.Fatal("renewal changed serving state or lost failure history")
			}
			requireRenewalResponse(t, transport, server.URL+"/response")
		})
	}
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
			server, key := renewalServer(t, nil)
			em, model, previous := renewalManager(t, server, time.Now(), time.Now(), func(string, string) (*verify.Verification, error) {
				return renewalVerification(key, tc.expiresAt), nil
			})
			delete(model.Enclaves, previous.host)
			if err := em.addEnclave(renewalTestModel, previous.host); err == nil {
				t.Fatal("accepted verification without a valid authenticated deadline")
			}
			if len(model.Enclaves) != 0 || model.Tag != "v1.0.0" || model.SourceMeasurement != nil {
				t.Fatalf("invalid deadline modified model state: enclaves=%d tag=%q measurement=%v", len(model.Enclaves), model.Tag, model.SourceMeasurement)
			}
		})
	}
}
