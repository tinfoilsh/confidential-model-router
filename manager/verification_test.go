package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go"
	"github.com/tinfoilsh/tinfoil-go/verify/measurement"

	"github.com/tinfoilsh/confidential-model-router/config"
)

const (
	renewalTestModel   = "gpt-oss-120b"
	renewalTestRepo    = "tinfoilsh/confidential-gpt-oss-120b"
	renewalTestProject = "tinfoil-inference/gpt-oss-120b"
	renewalTestHost    = "backend.example"
	renewalTestTimeout = 5 * time.Second
	renewalTestRefresh = time.Minute
)

// stubClient stands in for the SDK's enclave client: it holds a verification
// and replaces it with renewed when asked to verify again.
type stubClient struct {
	mu         sync.Mutex
	verified   attestation
	renewed    attestation
	err        error
	reverifies int
}

func (c *stubClient) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the stub client sends nothing")
}

func (c *stubClient) current() attestation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verified
}

func (c *stubClient) reverify(context.Context) (attestation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reverifies++
	if c.err != nil {
		return attestation{}, c.err
	}
	c.verified = c.renewed
	return c.verified, nil
}

func testAttestation(expiresAt, verifiedAt time.Time, codeTag string) *attestation {
	return &attestation{expiresAt: expiresAt, verifiedAt: verifiedAt, codeTag: codeTag}
}

// renewalManager returns a manager whose one model already serves through
// client, as it would after a first verification.
func renewalManager(t *testing.T, client *stubClient) (*EnclaveManager, *Model, *Enclave) {
	t.Helper()
	existing := newTestEnclave(renewalTestHost)
	existing.modelName = renewalTestModel
	existing.client = client
	verified := client.current()
	t.Cleanup(existing.shutdown)
	model := &Model{
		Repo:            renewalTestRepo,
		Tag:             verified.codeTag,
		Enclaves:        map[string]*Enclave{renewalTestHost: existing},
		expectedHosts:   1,
		configuredHosts: map[string]verificationAttempt{renewalTestHost: {}},
	}
	em := &EnclaveManager{models: &sync.Map{}, refreshInterval: renewalTestRefresh, connect: func(context.Context, string, tinfoil.Pin) (enclaveClient, error) {
		t.Error("a host that is already served was connected again")
		return nil, errors.New("unexpected connection")
	}}
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

func TestAttestationRefreshSchedule(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                  string
		expiresIn, verifiedIn time.Duration
		due                   bool
	}{
		{"fresh", 2 * attestationRenewalWindow, -time.Minute, false},
		{"inside the renewal window", attestationRenewalWindow / 2, -time.Minute, true},
		{"expired", -time.Second, -time.Minute, true},
		{"verified an hour ago", 2 * attestationRenewalWindow, -attestationRefreshInterval, true},
		{"verified within a refresh of an hour ago", 2 * attestationRenewalWindow, -attestationRefreshInterval + renewalTestRefresh/2, true},
	} {
		verified := attestation{expiresAt: now.Add(tc.expiresIn), verifiedAt: now.Add(tc.verifiedIn)}
		if due := verified.refreshDue(now, renewalTestRefresh); due != tc.due {
			t.Errorf("%s: refresh due = %v, want %v", tc.name, due, tc.due)
		}
	}
}

func TestAddEnclaveConnectsAHostOnce(t *testing.T) {
	now := time.Now()
	launch, _ := measurement.ParseExpectedSEVSNP(strings.Repeat("ab", measurement.RegisterSize))
	client := &stubClient{verified: attestation{expiresAt: now.Add(2 * attestationRenewalWindow), verifiedAt: now, codeTag: "v1.0.0", codeMeasurement: launch, tlsKeyFP: "tls-key"}}
	connections := 0
	model := &Model{Repo: renewalTestRepo, Enclaves: map[string]*Enclave{}, configuredHosts: map[string]verificationAttempt{renewalTestHost: {}}}
	em := &EnclaveManager{models: &sync.Map{}, refreshInterval: renewalTestRefresh, connect: func(_ context.Context, host string, pin tinfoil.Pin) (enclaveClient, error) {
		if host != renewalTestHost || pin != (tinfoil.Pin{Repo: renewalTestRepo}) {
			t.Errorf("incorrect verification target: host=%q pin=%+v", host, pin)
		}
		connections++
		return client, nil
	}}
	em.models.Store(renewalTestModel, model)

	for range 2 {
		if err := em.addEnclave(renewalTestModel, renewalTestHost); err != nil {
			t.Fatal(err)
		}
	}
	enclave := model.Enclaves[renewalTestHost]
	t.Cleanup(enclave.shutdown)
	if connections != 1 || client.reverifies != 0 {
		t.Fatalf("fresh host verified again: connections=%d reverifications=%d", connections, client.reverifies)
	}
	if enclave.client != client || enclave.client.current().tlsKeyFP != "tls-key" || model.Tag != "v1.0.0" || model.SourceMeasurement != launch {
		t.Fatalf("verified state not published: tag=%q measurement=%v", model.Tag, model.SourceMeasurement)
	}
}

// The pin carries the model's trust anchors as configured: an anchor that is
// not set stays empty, and a version pin reaches the verifier unchanged.
func TestEnclavesAreVerifiedAgainstTheConfiguredTrustAnchors(t *testing.T) {
	const pinnedProject = renewalTestProject + "@v3"
	for _, tc := range []struct {
		name, anchors string
		want          tinfoil.Pin
	}{
		{"repository only", "repo: " + renewalTestRepo, tinfoil.Pin{Repo: renewalTestRepo}},
		{"project only", "project: " + renewalTestProject, tinfoil.Pin{Project: renewalTestProject}},
		{"both", "repo: " + renewalTestRepo + "\n    project: " + renewalTestProject, tinfoil.Pin{Repo: renewalTestRepo, Project: renewalTestProject}},
		{"project revision", "project: " + pinnedProject, tinfoil.Pin{Project: pinnedProject}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.FromBytes([]byte(fmt.Sprintf("models:\n  %s:\n    %s\n    enclaves: [%s]\n", renewalTestModel, tc.anchors, renewalTestHost)))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			var pins []tinfoil.Pin
			em := &EnclaveManager{models: &sync.Map{}, refreshInterval: renewalTestRefresh, connect: func(_ context.Context, _ string, pin tinfoil.Pin) (enclaveClient, error) {
				pins = append(pins, pin)
				return &stubClient{verified: *testAttestation(now.Add(2*attestationRenewalWindow), now, "v1.0.0")}, nil
			}}
			em.addModel(renewalTestModel, cfg.Models[renewalTestModel])
			if err := em.addEnclave(renewalTestModel, renewalTestHost); err != nil {
				t.Fatal(err)
			}
			model, _ := em.GetModel(renewalTestModel)
			t.Cleanup(model.Enclaves[renewalTestHost].shutdown)
			if len(pins) != 1 || pins[0] != tc.want {
				t.Fatalf("verified against %+v, want %+v", pins, tc.want)
			}
		})
	}
}

// While addEnclave is blocked on an unresponsive host, the model lock must
// remain available to request routing.
func TestAddEnclaveNetworkIODoesNotHoldModelLock(t *testing.T) {
	started, blocked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	model := &Model{Repo: renewalTestRepo, Enclaves: map[string]*Enclave{}, configuredHosts: map[string]verificationAttempt{renewalTestHost: {}}}
	em := &EnclaveManager{models: &sync.Map{}, connect: func(context.Context, string, tinfoil.Pin) (enclaveClient, error) {
		close(started)
		<-blocked
		return nil, errors.New("attestation unavailable")
	}}
	em.models.Store(renewalTestModel, model)

	done := make(chan struct{})
	var verificationError error
	go func() {
		defer close(done)
		verificationError = em.addEnclave(renewalTestModel, renewalTestHost)
	}()
	t.Cleanup(func() { release(); <-done })

	waitRenewalSignal(t, started)
	locked := make(chan struct{})
	go func() {
		model.mu.Lock()
		if len(model.Enclaves) != 0 {
			t.Error("enclave inserted before verification completed")
		}
		model.mu.Unlock()
		close(locked)
	}()
	waitRenewalSignal(t, locked)

	release()
	waitRenewalSignal(t, done)
	if verificationError == nil || len(model.Enclaves) != 0 || model.configuredHosts[renewalTestHost].failures != 1 {
		t.Fatalf("failed verification was accepted or not recorded: %v", verificationError)
	}
}

func TestAttestationRenewalPreservesServingState(t *testing.T) {
	now := time.Now()
	newExpiry := now.Add(2 * attestationRenewalWindow)
	client := &stubClient{
		verified: *testAttestation(now.Add(time.Hour), now.Add(-time.Minute), "v1.0.0"),
		renewed:  *testAttestation(newExpiry, now, "v1.0.1"),
	}
	em, model, existing := renewalManager(t, client)
	oldProxy, oldBreaker, oldMetrics := existing.proxy, existing.cb, existing.metrics
	reserveTestDispatch(t, existing)
	defer existing.ReleaseDispatch()
	tripBreaker(existing)
	existing.cb.lastFailureNano.Store(now.Add(-cbCooldown - time.Second).UnixNano())
	if existing.claimProbe() == nil {
		t.Fatal("expected a claimed recovery probe before renewal")
	}

	if err := em.addEnclave(renewalTestModel, renewalTestHost); err != nil {
		t.Fatal(err)
	}
	current := model.Enclaves[renewalTestHost]
	if current != existing || current.proxy != oldProxy || current.cb != oldBreaker || current.metrics != oldMetrics {
		t.Fatal("renewal replaced serving state")
	}
	if current.inflight.Load() != 1 || current.cb.Retired() || current.cb.State() != cbHalfOpen || current.cb.ConsecutiveFailures() != cbFailureThreshold {
		t.Fatal("renewal lost active runtime state")
	}
	if updated := current.client.current(); client.reverifies != 1 || !updated.expiresAt.Equal(newExpiry) || updated.codeTag != "v1.0.1" || model.Tag != "v1.0.1" {
		t.Fatalf("renewal did not publish verified evidence: %+v", updated)
	}
}

func TestAttestationRenewalFailurePreservesEvidence(t *testing.T) {
	now := time.Now()
	client := &stubClient{verified: *testAttestation(now.Add(attestationRenewalWindow/2), now.Add(-time.Minute), "v1.0.0"), err: errors.New("collateral unavailable")}
	em, model, existing := renewalManager(t, client)
	before := existing.client.current()
	if err := em.addEnclave(renewalTestModel, renewalTestHost); err == nil {
		t.Fatal("failed renewal was accepted")
	}
	if model.Enclaves[renewalTestHost] != existing || existing.client.current() != before || existing.cb.Retired() {
		t.Fatal("failed renewal replaced the enclave or its evidence")
	}
	if attempt := model.configuredHosts[renewalTestHost]; attempt.lastError != verificationVerify || attempt.failures != 1 {
		t.Fatalf("verification failure recorded as %+v", attempt)
	}
}

// The client verifies again by itself when an enclave restarts with a new
// key. The next refresh publishes what the client then holds.
func TestRefreshAdoptsTheClientsVerification(t *testing.T) {
	now := time.Now()
	client := &stubClient{verified: *testAttestation(now.Add(2*attestationRenewalWindow), now, "v1.0.0")}
	em, model, existing := renewalManager(t, client)
	restarted := *testAttestation(now.Add(3*attestationRenewalWindow), now, "v1.0.1")
	restarted.tlsKeyFP = "new-tls-key"
	client.mu.Lock()
	client.verified = restarted
	client.mu.Unlock()

	if err := em.addEnclave(renewalTestModel, renewalTestHost); err != nil {
		t.Fatal(err)
	}
	if adopted := existing.client.current(); client.reverifies != 0 || adopted.tlsKeyFP != "new-tls-key" || model.Tag != "v1.0.1" || model.Enclaves[renewalTestHost] != existing {
		t.Fatalf("refresh did not adopt the client's verification: %+v", adopted)
	}
	if attempt := model.configuredHosts[renewalTestHost]; !attempt.lastAttempt.IsZero() {
		t.Fatal("adopting a verification was recorded as an attempt")
	}
}

func TestAttestationRenewalUsesAcceptedConfiguration(t *testing.T) {
	const configMode = 0o600
	now := time.Now()
	newExpiry := now.Add(time.Hour)
	client := &stubClient{
		verified: *testAttestation(now.Add(time.Minute), now.Add(-attestationRefreshInterval-time.Second), "v1.0.0"),
		renewed:  *testAttestation(newExpiry, now.Add(-attestationRefreshInterval-time.Second), "v1.0.1"),
	}
	em, model, existing := renewalManager(t, client)
	model.configuredHosts[renewalTestHost] = verificationAttempt{failures: 1}
	path := t.TempDir() + "/config.yaml"
	acceptedConfig := fmt.Sprintf("models:\n  %s:\n    repo: %s\n    enclaves: [%q]\n", renewalTestModel, renewalTestRepo, renewalTestHost)
	for _, tc := range []struct {
		name, config string
		retained     bool
	}{
		{"accepted", acceptedConfig, true},
		{"unavailable", "", true},
		{"repository change", strings.NewReplacer(renewalTestRepo, "tinfoilsh/untrusted", renewalTestHost, "untrusted.example").Replace(acceptedConfig), true},
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
			beforeConfig, beforeCalls := em.lastSuccessfulUpdate, client.reverifies
			if err := em.sync(); (err != nil) != (tc.config == "") {
				t.Fatalf("unexpected config fetch result: %v", err)
			}
			if (tc.config == "") != em.lastSuccessfulUpdate.Equal(beforeConfig) {
				t.Fatal("configuration success timestamp does not match fetch result")
			}
			if !tc.retained {
				if client.reverifies != beforeCalls || len(model.configuredHosts) != 0 || len(model.Enclaves) != 0 || !existing.cb.Retired() {
					t.Fatal("removed backend was retained or renewed")
				}
				return
			}
			if client.reverifies != beforeCalls+1 || !existing.client.current().expiresAt.Equal(newExpiry) {
				t.Fatal("accepted backend did not renew exactly once")
			}
			if len(model.Enclaves) != 1 || model.Enclaves[renewalTestHost] != existing || len(model.configuredHosts) != 1 || model.configuredHosts[renewalTestHost].failures != 1 {
				t.Fatal("renewal changed serving state or lost failure history")
			}
		})
	}
}
