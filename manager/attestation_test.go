package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestV3AllowsDifferentEndorsedVersionsInOneModel(t *testing.T) {
	em, model := attestationTestManager(t, "older", "newer")
	em.verifyEnclave = func(host, repo string) (*tinfoilClient.VerifiedDocumentV3, error) {
		return testVerification(host, host, time.Now().Add(time.Hour)), nil
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

func TestDiscoveryOmitsExpiredEndpoints(t *testing.T) {
	model := newTestModel("valid", "expired")
	for _, e := range model.Enclaves {
		t.Cleanup(e.shutdown)
	}
	setTestAttestation(model.Enclaves["expired"], time.Now().Add(-time.Second))
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
	if len(status.Enclaves) != 1 || status.Enclaves["valid"] == nil {
		t.Fatal("discovery advertised expired endpoint")
	}
}

func TestConfigFailureStillReattestsKnownTargetsWithBoundedConcurrency(t *testing.T) {
	var hosts []string
	for i := range 12 {
		hosts = append(hosts, fmt.Sprintf("host-%d", i))
	}
	em, model := attestationTestManager(t, hosts...)
	em.updateConfigURL = filepath.Join(t.TempDir(), "missing-config.yml")
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
		time.Sleep(time.Millisecond)
		return testVerification("v1", host, time.Now().Add(time.Hour)), nil
	}
	if err := em.sync(); err == nil {
		t.Fatal("configuration failure was swallowed")
	}
	if calls.Load() != int32(len(hosts)) || len(model.Enclaves) != len(hosts) {
		t.Fatal("configuration failure suppressed reattestation")
	}
	if maximum.Load() > attestationConcurrency {
		t.Fatalf("unbounded verification concurrency: %d", maximum.Load())
	}
}
