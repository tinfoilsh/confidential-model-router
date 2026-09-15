package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
	"github.com/tinfoilsh/tinfoil-go/verifier/envelope"
	"github.com/tinfoilsh/tinfoil-go/verifier/measurement"
)

func setTestAttestation(e *Enclave, deadline time.Time) {
	e.verification.Store(&tinfoilClient.VerifiedDocumentV3{FreshnessExpiresAt: deadline})
}

func testVerification(tag, key string, deadline time.Time) *tinfoilClient.VerifiedDocumentV3 {
	return &tinfoilClient.VerifiedDocumentV3{
		CodeTag: tag, CodeDigest: "digest-" + tag,
		CodeMeasurement:    &measurement.Measurement{Type: measurement.SnpTdxMultiPlatformV1},
		EnclaveMeasurement: &measurement.Measurement{Type: measurement.SevGuestV2},
		FreshnessExpiresAt: deadline,
		CryptoMaterial: []envelope.CryptoMaterialItem{
			{ID: envelope.CryptoMaterialIDTLS, Format: envelope.KeySPKIFPSHA256V1Format, Data: key},
			{ID: envelope.CryptoMaterialIDHPKE, Format: envelope.KeyX25519HPKEV1Format, Data: "hpke-" + key},
		},
	}
}

func attestationTestManager(t *testing.T, hosts ...string) (*EnclaveManager, *Model) {
	t.Helper()
	em := &EnclaveManager{models: &sync.Map{}}
	em.addModel("test-model", config.Model{Repo: "tinfoilsh/expected-code", Hostnames: hosts})
	model, _ := em.GetModel("test-model")
	t.Cleanup(em.Shutdown)
	return em, model
}

func TestV3RenewalPreservesStateAndDoesNotExtendUnchangedProof(t *testing.T) {
	em, model := attestationTestManager(t, "backend")
	deadline := time.Now().Add(time.Hour)
	verified := testVerification("v1", "key", deadline)
	calls := 0
	em.verifyEnclave = func(host, repo string) (*tinfoilClient.VerifiedDocumentV3, error) {
		calls++
		if host != "backend" || repo != "tinfoilsh/expected-code" {
			t.Fatalf("unexpected trust inputs: %s %s", host, repo)
		}
		return verified, nil
	}
	if err := em.addEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	original := model.Enclaves["backend"]
	original.cb.RecordFailure()
	original.inflight.Store(3)
	if err := em.addEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || model.Enclaves["backend"] != original {
		t.Fatal("same-key endpoint was not reverified in place")
	}
	if !original.verification.Load().FreshnessExpiresAt.Equal(deadline) || original.cb.ConsecutiveFailures() != 1 || original.inflight.Load() != 3 {
		t.Fatal("reverification extended the witness or reset live state")
	}
	// A renewed witness advances the deadline without rebuilding the proxy.
	verified = testVerification("v1", "key", deadline.Add(time.Hour))
	if err := em.addEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if model.Enclaves["backend"] != original || !original.verification.Load().FreshnessExpiresAt.Equal(verified.FreshnessExpiresAt) {
		t.Fatal("renewal did not update the existing endpoint")
	}
	// A rotated key retires old clients as well as replacing the route.
	verified = testVerification("v2", "rotated-key", deadline.Add(time.Hour))
	if err := em.addEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if model.Enclaves["backend"] == original || original.attestationValid() || !original.cb.Retired() {
		t.Fatal("key rotation failed to retire previous endpoint")
	}
}

func TestHealthyReattestationIsDaily(t *testing.T) {
	em, model := attestationTestManager(t, "backend")
	deadline := time.Now().Add(72 * time.Hour)
	verifications, probes := 0, 0
	em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) {
		verifications++
		return testVerification("v1", "key", deadline), nil
	}
	em.probeTLSKey = func(string) (string, error) { probes++; return "key", nil }
	before := time.Now()
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	e := model.Enclaves["backend"]
	if e.nextAttestationAt.Before(before.Add(24*time.Hour)) || e.nextAttestationAt.After(time.Now().Add(24*time.Hour)) {
		t.Fatal("healthy backend was not scheduled for daily reattestation")
	}
	scheduled := e.nextAttestationAt
	for range 3 {
		if err := em.refreshEnclave("test-model", "backend"); err != nil {
			t.Fatal(err)
		}
	}
	if verifications != 1 || probes != 3 || !e.nextAttestationAt.Equal(scheduled) {
		t.Fatal("healthy ticks re-attested or postponed the daily check")
	}
	e.nextAttestationAt = time.Now().Add(-time.Second)
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if verifications != 2 || probes != 3 || model.Enclaves["backend"] != e {
		t.Fatal("daily check did not reverify the unchanged key in place")
	}
}

func TestReattestationRunsBeforeProofExpiry(t *testing.T) {
	em, model := attestationTestManager(t, "backend")
	deadline := time.Now().Add(6 * time.Hour)
	verifications := 0
	em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) {
		verifications++
		return testVerification("v1", "key", deadline), nil
	}
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	e := model.Enclaves["backend"]
	if !e.nextAttestationAt.Equal(deadline.Add(-time.Hour)) {
		t.Fatal("short-lived proof was not scheduled one hour before expiry")
	}
	// Receiving an already-near-expiry proof must retry on subsequent worker
	// ticks, rather than scheduling another 24-hour wait or extending expiry.
	deadline = time.Now().Add(30 * time.Minute)
	if err := em.addEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if verifications != 3 || !e.verification.Load().FreshnessExpiresAt.Equal(deadline) || e.nextAttestationAt.After(time.Now()) {
		t.Fatal("unchanged near-expiry proof did not remain due for renewal")
	}
	deadline = time.Now().Add(72 * time.Hour)
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if !e.nextAttestationAt.After(time.Now()) || !e.verification.Load().FreshnessExpiresAt.Equal(deadline) {
		t.Fatal("renewed proof did not restore the normal schedule")
	}
	// Expiry is also independently due, even if the scheduling timestamp is
	// unexpectedly still in the future.
	setTestAttestation(e, time.Now().Add(-time.Second))
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if verifications != 5 || !e.attestationValid() {
		t.Fatal("expired endpoint was not reverified")
	}
}

func TestDueReattestationRetriesAfterFetchFailure(t *testing.T) {
	em, model := attestationTestManager(t, "backend")
	verified := testVerification("v1", "key", time.Now().Add(72*time.Hour))
	var fetchErr error
	calls := 0
	em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) {
		calls++
		return verified, fetchErr
	}
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	e := model.Enclaves["backend"]
	e.nextAttestationAt = time.Now().Add(-time.Second)
	fetchErr = attestationFetchError{errors.New("timeout")}
	for range 2 {
		if err := em.refreshEnclave("test-model", "backend"); err == nil {
			t.Fatal("fetch error was swallowed")
		}
		if !e.attestationValid() || !e.verification.Load().FreshnessExpiresAt.Equal(verified.FreshnessExpiresAt) || e.nextAttestationAt.After(time.Now()) {
			t.Fatal("failed check changed validity or postponed the retry")
		}
	}
	fetchErr = nil
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || !e.nextAttestationAt.After(time.Now()) {
		t.Fatal("due endpoint did not recover on the next check")
	}
}

func TestTLSKeyChangeTriggersEarlyV3Verification(t *testing.T) {
	em, model := attestationTestManager(t, "backend")
	key := "old-key"
	calls := 0
	em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) {
		calls++
		return testVerification("v1", key, time.Now().Add(72*time.Hour)), nil
	}
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	original := model.Enclaves["backend"]
	scheduled := original.nextAttestationAt
	em.probeTLSKey = func(string) (string, error) { return "", errors.New("temporary TLS failure") }
	if err := em.refreshEnclave("test-model", "backend"); err == nil {
		t.Fatal("probe error was swallowed")
	}
	if calls != 1 || !original.attestationValid() || !original.nextAttestationAt.Equal(scheduled) {
		t.Fatal("probe failure altered a valid endpoint's verification")
	}
	key = "new-key"
	em.probeTLSKey = func(string) (string, error) { return key, nil }
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || model.Enclaves["backend"] == original || original.attestationValid() || model.Enclaves["backend"].tlsKeyFP != key {
		t.Fatal("key change did not trigger early authenticated replacement")
	}
}

func TestTLSKeyProbeIsBounded(t *testing.T) {
	shortVerifyTimeout(t, 50*time.Millisecond)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release, serverDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-release // Accept TCP but never complete the TLS handshake.
	}()
	probeDone := make(chan struct{})
	var probeErr error
	go func() {
		_, probeErr = tlsPublicKeyFP(listener.Addr().String())
		close(probeDone)
	}()
	defer func() {
		// Unblock the probe even if its production timeout is accidentally removed.
		close(release)
		listener.Close()
		<-serverDone
		<-probeDone
	}()
	select {
	case <-probeDone:
		var timeout net.Error
		if !errors.As(probeErr, &timeout) || !timeout.Timeout() {
			t.Fatalf("expected TLS probe timeout, got %v", probeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS probe exceeded its 50ms deadline plus scheduling slack")
	}
}

func TestTLSKeyProbeDoesNotHoldModelLock(t *testing.T) {
	em, model := attestationTestManager(t, "backend")
	em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) {
		return testVerification("v1", "key", time.Now().Add(72*time.Hour)), nil
	}
	if err := em.refreshEnclave("test-model", "backend"); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	em.probeTLSKey = func(string) (string, error) { close(started); <-release; return "key", nil }
	done := make(chan error, 1)
	defer func() {
		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("probe did not finish after release")
		}
	}()
	go func() { done <- em.refreshEnclave("test-model", "backend") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	locked := make(chan struct{})
	go func() { model.mu.Lock(); model.mu.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("probe held the model lock")
	}
}

func TestV3AllowsDifferentEndorsedVersionsInOneModel(t *testing.T) {
	em, model := attestationTestManager(t, "older", "newer")
	deadline := time.Now().Add(time.Hour)
	expected := map[string]*tinfoilClient.VerifiedDocumentV3{
		"older": testVerification("older", "older", deadline),
		"newer": testVerification("newer", "newer", deadline.Add(time.Hour)),
	}
	for host, verified := range expected {
		verified.CodeMeasurement.Registers = []string{"measurement-" + host}
	}
	em.verifyEnclave = func(host, repo string) (*tinfoilClient.VerifiedDocumentV3, error) {
		return expected[host], nil
	}
	em.refreshAttestations()
	if len(model.Enclaves) != 2 || model.Enclaves["older"].verification.Load().CodeTag != "older" || model.Enclaves["newer"].verification.Load().CodeTag != "newer" {
		t.Fatal("per-endpoint endorsed versions were not retained")
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &status); err != nil {
		t.Fatal(err)
	}
	if status["tag"] != nil || status["measurement"] != nil {
		t.Fatal("status still advertises a single model-wide release")
	}
	var endpoints map[string]struct {
		Tag                string                   `json:"tag"`
		Digest             string                   `json:"digest"`
		Measurement        *measurement.Measurement `json:"measurement"`
		FreshnessExpiresAt time.Time                `json:"freshness_expires_at"`
	}
	if err := json.Unmarshal(status["enclaves"], &endpoints); err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != len(expected) {
		t.Fatalf("serialized %d endpoints, want %d", len(endpoints), len(expected))
	}
	for host, want := range expected {
		got := endpoints[host]
		if got.Tag != want.CodeTag || got.Digest != want.CodeDigest ||
			!reflect.DeepEqual(got.Measurement, want.CodeMeasurement) || !got.FreshnessExpiresAt.Equal(want.FreshnessExpiresAt) {
			t.Errorf("endpoint %s lost its verified release metadata: %+v", host, got)
		}
	}
}

func TestV3FailureHandling(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		keep bool
	}{
		{"temporary fetch failure", attestationFetchError{errors.New("timeout")}, true},
		{"invalid evidence", errors.New("invalid signature"), false},
		{"wrong repository", errors.New("signing identity mismatch"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			em, model := attestationTestManager(t, "backend")
			deadline := time.Now().Add(time.Hour)
			em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) {
				return testVerification("v1", "key", deadline), nil
			}
			if err := em.addEnclave("test-model", "backend"); err != nil {
				t.Fatal(err)
			}
			previous := model.Enclaves["backend"]
			em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) { return nil, tc.err }
			if err := em.addEnclave("test-model", "backend"); err == nil {
				t.Fatal("verification failure was swallowed")
			}
			if got := model.Enclaves["backend"] != nil; got != tc.keep || previous.attestationValid() != tc.keep {
				t.Fatalf("kept endpoint = %v, want %v", got, tc.keep)
			}
			if tc.keep {
				if !previous.verification.Load().FreshnessExpiresAt.Equal(deadline) {
					t.Fatal("fetch failure extended validity")
				}
				setTestAttestation(previous, time.Now().Add(-time.Second))
				if err := em.addEnclave("test-model", "backend"); err == nil {
					t.Fatal("expired endpoint fetch failure was swallowed")
				}
				if selected, _ := model.NextEnclave(nil); selected != nil {
					t.Fatal("fetch failure allowed an expired endpoint to serve")
				}
			}
		})
	}
}

func TestV3RejectsMissingExpiredAndIncompleteResults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verified *tinfoilClient.VerifiedDocumentV3
	}{
		{"missing", nil},
		{"zero deadline", testVerification("v1", "key", time.Time{})},
		{"expired", testVerification("v1", "key", time.Now().Add(-time.Second))},
		{"missing keys", &tinfoilClient.VerifiedDocumentV3{FreshnessExpiresAt: time.Now().Add(time.Hour), EnclaveMeasurement: &measurement.Measurement{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			em, model := attestationTestManager(t, "backend")
			em.verifyEnclave = func(string, string) (*tinfoilClient.VerifiedDocumentV3, error) { return tc.verified, nil }
			if err := em.addEnclave("test-model", "backend"); err == nil || len(model.Enclaves) != 0 {
				t.Fatal("incomplete verification admitted an endpoint")
			}
		})
	}
}

func TestExpiryIsAHardRoutingGate(t *testing.T) {
	model := newTestModel("expired")
	e := model.Enclaves["expired"]
	t.Cleanup(e.shutdown)
	for _, absent := range []bool{false, true} {
		setTestAttestation(e, time.Now().Add(-time.Second))
		if absent {
			e.verification.Store(nil)
		}
		tripBreaker(e)
		e.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
		if got, claim := model.NextEnclave(nil); got != nil || claim != nil {
			t.Fatal("expired fallback/probe selected")
		}
		if got, claim := model.NextEnclavePreferring([]string{"expired"}, nil); got != nil || claim != nil {
			t.Fatal("expired preferred endpoint selected")
		}
		if got, claim := model.SelectForDispatch([]string{"expired"}); got != nil || claim != nil {
			t.Fatal("expired internal endpoint selected")
		}
		if got, claim := model.SelectForDispatchPools(nil, map[string]bool{"expired": true}, nil); got != nil || claim != nil {
			t.Fatal("expired reserved endpoint selected")
		}
		if got, claim, _, _, _ := model.SelectServing(nil, nil, nil); got != nil || claim != nil {
			t.Fatal("expired serving endpoint selected")
		}
		if model.HasHealthyEnclave() || len(model.CacheRoutePool().Candidates) != 0 {
			t.Fatal("expired endpoint advertised as eligible")
		}
		response := httptest.NewRecorder()
		e.ServeHTTP(response, httptest.NewRequest("POST", "/", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatal("direct dispatch did not reject expired attestation")
		}
	}
}

type attestationTestTransport func(*http.Request) (*http.Response, error)

func (f attestationTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCachedTransportChecksCurrentValidityAndRetirement(t *testing.T) {
	e := newTestEnclave("backend")
	t.Cleanup(e.shutdown)
	calls := 0
	transport := &attestationTransport{enclave: e, base: attestationTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	request := httptest.NewRequest("GET", "https://backend/", nil)
	resp, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	// Already-started streams remain readable after the authorization expires.
	setTestAttestation(e, time.Now().Add(-time.Second))
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "ok" {
		t.Fatal("existing response was interrupted")
	}
	resp.Body.Close()
	if _, err := transport.RoundTrip(request); err == nil || calls != 1 {
		t.Fatal("cached transport sent after expiry")
	}
	setTestAttestation(e, time.Now().Add(time.Hour))
	resp, err = transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	e.shutdown()
	if _, err := transport.RoundTrip(request); err == nil || calls != 2 {
		t.Fatal("cached client survived retirement")
	}
}

func TestDiscoveryJSONDoesNotRestoreAuthorization(t *testing.T) {
	e := newTestEnclave("backend")
	t.Cleanup(e.shutdown)
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Enclave
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.attestationValid() {
		t.Fatal("untrusted discovery metadata restored authorization")
	}
}

func TestDiscoveryOmitsIneligibleEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hosts []string
		want  []string
	}{
		{"mixed", []string{"valid", "expired", "unverified", "retired"}, []string{"valid"}},
		{"all ineligible", []string{"expired", "unverified", "retired"}, nil},
		{"empty", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newTestModel(tc.hosts...)
			for host, e := range model.Enclaves {
				t.Cleanup(e.shutdown)
				switch host {
				case "expired":
					setTestAttestation(e, time.Now().Add(-time.Second))
				case "unverified":
					e.verification.Store(nil)
				case "retired":
					e.shutdown()
				}
			}
			em := &EnclaveManager{models: &sync.Map{}}
			em.models.Store("test-model", model)
			data, err := json.Marshal(model)
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Enclaves map[string]json.RawMessage `json:"enclaves"`
			}
			if err := json.Unmarshal(data, &status); err != nil {
				t.Fatal(err)
			}
			if len(status.Enclaves) != len(tc.want) {
				t.Fatalf("discovery endpoints = %v, want %v", status.Enclaves, tc.want)
			}
			for _, host := range tc.want {
				if status.Enclaves[host] == nil {
					t.Errorf("discovery omitted eligible endpoint %s", host)
				}
			}
			var wantGroups []PrometheusTargetGroup
			if len(tc.want) > 0 {
				wantGroups = []PrometheusTargetGroup{{
					Targets: tc.want,
					Labels:  map[string]string{"model_name": "test-model", "__param_model": "test-model"},
				}}
			}
			if got := em.PrometheusTargets(); !reflect.DeepEqual(got, wantGroups) {
				t.Fatalf("Prometheus discovery = %+v, want %+v", got, wantGroups)
			}
		})
	}
}

func TestConfigFailureStillReattestsKnownTargetsWithBoundedConcurrency(t *testing.T) {
	var hosts []string
	for i := range 12 {
		hosts = append(hosts, fmt.Sprintf("host-%d", i))
	}
	em, model := attestationTestManager(t, hosts...)
	em.updateConfigURL = filepath.Join(t.TempDir(), "missing-config.yml")
	entered, release := make(chan struct{}, len(hosts)), make(chan struct{})
	releaseChecks := sync.OnceFunc(func() { close(release) })
	var calls, active, maximum atomic.Int32
	em.verifyEnclave = func(host, repo string) (*tinfoilClient.VerifiedDocumentV3, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return testVerification("v1", host, time.Now().Add(time.Hour)), nil
	}
	done := make(chan struct{})
	var syncErr error
	go func() {
		syncErr = em.sync()
		close(done)
	}()
	defer func() { releaseChecks(); <-done }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for range attestationConcurrency {
		select {
		case <-entered:
		case <-deadline.C:
			t.Fatal("verification workers did not overlap")
		}
	}
	releaseChecks()
	<-done
	if syncErr == nil {
		t.Fatal("configuration failure was swallowed")
	}
	if calls.Load() != int32(len(hosts)) || len(model.Enclaves) != len(hosts) {
		t.Fatal("configuration failure suppressed reattestation")
	}
	if maximum.Load() != attestationConcurrency {
		t.Fatalf("verification concurrency = %d, want %d", maximum.Load(), attestationConcurrency)
	}
}
