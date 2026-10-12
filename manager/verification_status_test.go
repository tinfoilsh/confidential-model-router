package manager

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/tinfoilsh/confidential-model-router/config"
)

func readVerificationStatus(t *testing.T, model *Model) modelVerificationStatus {
	t.Helper()
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Verification modelVerificationStatus `json:"verification"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result.Verification
}

func TestVerificationStatusCountsAndCompatibility(t *testing.T) {
	now := time.Now()
	model := &Model{
		Repo: "tinfoilsh/example", Tag: "v1", Overload: &config.OverloadConfig{MaxRequestsWaiting: 4},
		CacheRoute:      &config.CacheRouteConfig{Mode: "shadow"},
		Reservations:    []config.ReservationConfig{{OrgIDs: []string{"private-org"}, Enclaves: []string{"healthy"}}},
		configuredHosts: []string{"healthy", "open", "expired", "missing", "harness", "healthy"},
		Enclaves: map[string]*Enclave{
			"healthy": {tlsKeyFP: "tls-key", hpkeKey: "hpke-key", cb: newCircuitBreaker(), attestation: newAttestationState(now.Add(time.Hour), now, "v1")},
			"open":    {cb: newCircuitBreaker(), attestation: newAttestationState(now.Add(2*time.Hour), now, "v2")},
			"expired": {cb: newCircuitBreaker(), attestation: newAttestationState(now.Add(-time.Hour), now.Add(-2*time.Hour), "v0")},
			"harness": {cb: newCircuitBreaker()},
		},
	}
	model.Enclaves["open"].cb.storeState(cbOpen)
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-org") || strings.Contains(string(data), "reservations") {
		t.Fatalf("public status exposed reservations: %s", data)
	}
	var legacy Model
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatalf("legacy model decoder rejected status: %v", err)
	}
	if legacy.Repo != model.Repo || legacy.Tag != model.Tag || legacy.Overload.MaxRequestsWaiting != 4 || legacy.CacheRoute.Mode != "shadow" || legacy.Enclaves["healthy"].tlsKeyFP != "tls-key" || legacy.Enclaves["healthy"].hpkeKey != "hpke-key" {
		t.Fatalf("existing model fields changed: %s", data)
	}
	status := readVerificationStatus(t, model)
	if status.Configured != 5 || status.Verified != 2 || status.Healthy != 1 || status.Expired != 1 || status.Unverified != 2 {
		t.Fatalf("incorrect pool counts: %+v", status)
	}
	if len(status.Hosts) != 5 || status.Hosts["missing"].LastSuccess != nil || status.Hosts["harness"].ExpiresAt != nil {
		t.Fatalf("never-verified hosts must remain visible without authenticated timestamps: %+v", status.Hosts)
	}
	if status.EarliestFreshnessExpiresAt == nil || !status.EarliestFreshnessExpiresAt.Equal(now.Add(time.Hour)) || status.Hosts["open"].CodeTag != "v2" {
		t.Fatalf("missing authenticated per-host state: %+v", status)
	}
}

func TestVerificationStatusTracksFailureRecoveryAndRemoval(t *testing.T) {
	now := time.Now()
	manager := &EnclaveManager{}
	model := &Model{configuredHosts: []string{"backend"}, Enclaves: map[string]*Enclave{}}
	manager.recordVerification(model, "backend", now, verificationFetch, errors.New("secret upstream error"))
	status := readVerificationStatus(t, model)
	backend := status.Hosts["backend"]
	if backend.LastError != verificationFetch || backend.LastFailure == nil || !backend.LastFailure.Equal(now) || backend.LastAttempt == nil || !backend.LastAttempt.Equal(now) {
		t.Fatalf("failed attempt is missing: %+v", backend)
	}
	data, err := json.Marshal(model)
	if err != nil || strings.Contains(string(data), "secret upstream error") {
		t.Fatalf("public error was not sanitized: %s, %v", data, err)
	}
	manager.recordVerification(model, "backend", now.Add(time.Minute), verificationVerify, nil)
	backend = readVerificationStatus(t, model).Hosts["backend"]
	if backend.LastError != "" || backend.LastFailure == nil || !backend.LastFailure.Equal(now) || backend.LastAttempt == nil || !backend.LastAttempt.Equal(now.Add(time.Minute)) {
		t.Fatalf("successful retry did not clear error while preserving history: %+v", backend)
	}
	if model.verificationAttempts["backend"].failures[verificationFetch] != 1 {
		t.Fatal("successful retry discarded the failure counter")
	}
	model.configuredHosts = nil
	delete(model.verificationAttempts, "backend")
	manager.recordVerification(model, "backend", now.Add(2*time.Minute), verificationFetch, errors.New("late failure"))
	status = readVerificationStatus(t, model)
	if status.Configured != 0 || len(status.Hosts) != 0 || len(model.verificationAttempts) != 0 {
		t.Fatalf("removed host returned through an in-flight attempt: %+v", status)
	}
}

func verificationMetric(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matches := len(metric.Label) == len(labels)
			for _, label := range metric.Label {
				matches = matches && labels[label.GetName()] == label.GetValue()
			}
			if matches {
				if family.GetType() == dto.MetricType_COUNTER {
					return metric.GetCounter().GetValue()
				}
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("missing metric %s with labels %v", name, labels)
	return 0
}

func TestVerificationCollectorRecomputesExpirationAndDropsRemovedHosts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		manager := &EnclaveManager{models: &sync.Map{}}
		model := &Model{
			configuredHosts: []string{"backend", "missing"},
			Enclaves:        map[string]*Enclave{"backend": {cb: newCircuitBreaker(), attestation: newAttestationState(now.Add(time.Minute), now, "v1")}},
		}
		manager.models.Store("gpt-oss-120b", model)
		manager.recordVerification(model, "missing", now, verificationFetch, errors.New("unreachable"))
		registry := prometheus.NewRegistry()
		registry.MustRegister(manager.VerificationCollector())
		assertCount := func(state string, want float64) {
			t.Helper()
			if got := verificationMetric(t, registry, "router_verification_backends", map[string]string{"model": "gpt-oss-120b", "state": state}); got != want {
				t.Fatalf("%s count = %v, want %v", state, got, want)
			}
		}
		for state, count := range map[string]float64{"configured": 2, "verified": 1, "healthy": 1, "expired": 0, "unverified": 1} {
			assertCount(state, count)
		}
		labels := map[string]string{"model": "gpt-oss-120b", "enclave": "backend"}
		if got := verificationMetric(t, registry, "router_verification_last_success_timestamp_seconds", labels); got != float64(now.Unix()) {
			t.Fatalf("last success = %v, want %v", got, now.Unix())
		}
		if got := verificationMetric(t, registry, "router_verification_expires_timestamp_seconds", labels); got != float64(now.Add(time.Minute).Unix()) {
			t.Fatalf("expiry = %v, want %v", got, now.Add(time.Minute).Unix())
		}
		missing := map[string]string{"model": "gpt-oss-120b", "enclave": "missing"}
		if got := verificationMetric(t, registry, "router_verification_last_attempt_timestamp_seconds", missing); got != float64(now.Unix()) {
			t.Fatalf("last attempt = %v, want %v", got, now.Unix())
		}
		for _, stage := range verificationStages {
			want := float64(0)
			if stage == verificationFetch {
				want = 1
			}
			if got := verificationMetric(t, registry, "router_verification_failures_total", map[string]string{"model": "gpt-oss-120b", "enclave": "missing", "stage": stage}); got != want {
				t.Fatalf("%s failures = %v, want %v", stage, got, want)
			}
		}
		time.Sleep(time.Minute)
		assertCount("verified", 0)
		assertCount("healthy", 0)
		assertCount("expired", 1)
		status := readVerificationStatus(t, model)
		if status.Verified != 0 || status.Expired != 1 || status.EarliestFreshnessExpiresAt != nil {
			t.Fatalf("JSON status did not expire with the metric: %+v", status)
		}
		model.configuredHosts = []string{"missing"}
		delete(model.Enclaves, "backend")
		assertCount("configured", 1)
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "enclave" && label.GetValue() == "backend" {
						t.Fatalf("removed host retained in %s", family.GetName())
					}
				}
			}
		}
	})
}

func TestVerificationStatusConcurrentRefresh(t *testing.T) {
	const iterations = 100
	now := time.Now()
	manager := &EnclaveManager{models: &sync.Map{}}
	model := &Model{configuredHosts: []string{"backend"}, Enclaves: map[string]*Enclave{"backend": {attestation: newAttestationState(now.Add(time.Hour), now, "v1")}}}
	manager.models.Store("gpt-oss-120b", model)
	registry := prometheus.NewRegistry()
	registry.MustRegister(manager.VerificationCollector())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for range iterations {
			model.mu.Lock()
			model.Enclaves["backend"].attestation.current.Store(&verifiedAttestation{expiresAt: now.Add(time.Hour), verifiedAt: now, codeTag: "v2"})
			model.Enclaves["temporary"] = &Enclave{}
			model.configuredHosts = []string{"backend", "temporary"}
			model.mu.Unlock()
			manager.recordVerification(model, "backend", now, verificationFetch, errors.New("unreachable"))
			model.mu.Lock()
			delete(model.Enclaves, "temporary")
			model.configuredHosts = []string{"backend"}
			model.mu.Unlock()
		}
	}()
	for range iterations {
		status := readVerificationStatus(t, model)
		if status.Verified != 1 || status.Configured != status.Verified+status.Expired+status.Unverified || len(status.Hosts) != status.Configured {
			t.Fatalf("inconsistent status during refresh: %+v", status)
		}
		if _, err := registry.Gather(); err != nil {
			t.Fatal(err)
		}
	}
	workers.Wait()
}
