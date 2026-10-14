package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tinfoilsh/confidential-model-router/config"
)

func readVerificationStatus(t *testing.T, model *Model) (modelVerificationStatus, string) {
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
	return result.Verification, string(data)
}

func TestVerificationStatusLifecycle(t *testing.T) {
	const queueLimit = 4
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		manager := &EnclaveManager{models: &sync.Map{}}
		model := &Model{
			Repo: "tinfoilsh/example", Tag: "v1",
			Overload:        &config.OverloadConfig{MaxRequestsWaiting: queueLimit},
			CacheRoute:      &config.CacheRouteConfig{Mode: "shadow"},
			Reservations:    []config.ReservationConfig{{OrgIDs: []string{"private-org"}, Enclaves: []string{"backend"}}},
			configuredHosts: map[string]verificationAttempt{"backend": {}, "open": {}, "missing": {}, "harness": {}},
			Enclaves: map[string]*Enclave{
				"backend": {tlsKeyFP: "tls-key", hpkeKey: "hpke-key", cb: newCircuitBreaker(), attestation: newAttestationState(verifiedAttestation{expiresAt: now.Add(time.Minute), verifiedAt: now, codeTag: "v1"})},
				"open":    {cb: newCircuitBreaker(), attestation: newAttestationState(verifiedAttestation{expiresAt: now.Add(time.Minute), verifiedAt: now, codeTag: "v2"})},
				"harness": {},
			},
		}
		model.Enclaves["open"].cb.storeState(cbOpen)
		manager.models.Store("gpt-oss-120b", model)
		manager.recordVerification(model, "backend", now, verificationTLSProbe, errors.New("failed TLS probe"))
		manager.recordVerification(model, "backend", now, verificationVerify, errors.New("secret upstream error"))
		status, data := readVerificationStatus(t, model)
		if status.Configured != 4 || status.Verified != 2 || status.Healthy != 1 || status.Expired != 0 || status.Unverified != 2 || len(status.Hosts) != 4 {
			t.Fatalf("incorrect pool counts: %+v", status)
		}
		_, missingVisible := status.Hosts["missing"]
		if !missingVisible || status.Hosts["missing"].LastSuccess != nil || status.Hosts["harness"].ExpiresAt != nil || status.Hosts["backend"].LastError != verificationVerify || status.Hosts["open"].CodeTag != "v2" || status.EarliestFreshnessExpiresAt == nil || !status.EarliestFreshnessExpiresAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("incorrect authenticated state or failed-attempt details: %+v", status)
		}
		if strings.Contains(data, "private-org") || strings.Contains(data, "reservations") || strings.Contains(data, "secret upstream error") {
			t.Fatalf("public status exposed private details: %s", data)
		}
		var legacy Model
		if err := json.Unmarshal([]byte(data), &legacy); err != nil {
			t.Fatalf("legacy decoder rejected status: %v", err)
		}
		if legacy.Repo != model.Repo || legacy.Tag != model.Tag || legacy.Enclaves["backend"].tlsKeyFP != "tls-key" || legacy.Enclaves["backend"].hpkeKey != "hpke-key" {
			t.Fatalf("existing model fields changed: %s", data)
		}
		if legacy.Overload == nil || legacy.Overload.MaxRequestsWaiting != queueLimit || legacy.CacheRoute == nil || legacy.CacheRoute.Mode != model.CacheRoute.Mode {
			t.Fatalf("existing routing settings changed: %s", data)
		}
		registry := prometheus.NewRegistry()
		registry.MustRegister(manager.VerificationCollector())
		scrape := func() string {
			response := httptest.NewRecorder()
			promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
			return response.Body.String()
		}
		if metrics := scrape(); !strings.Contains(metrics, `router_verification_backends{model="gpt-oss-120b",state="verified"} 2`+"\n") {
			t.Fatalf("initial verified count missing from metrics:\n%s", metrics)
		}

		time.Sleep(time.Second)
		manager.recordVerification(model, "backend", time.Now(), verificationVerify, nil)
		status, _ = readVerificationStatus(t, model)
		backend := status.Hosts["backend"]
		if backend.LastError != "" || backend.LastFailure == nil || !backend.LastFailure.Equal(now) || backend.LastAttempt == nil || !backend.LastAttempt.Equal(time.Now()) {
			t.Fatalf("retry did not clear error while preserving history: %+v", backend)
		}
		time.Sleep(time.Minute - time.Second)
		status, _ = readVerificationStatus(t, model)
		if status.Verified != 0 || status.Healthy != 0 || status.Expired != 2 || status.EarliestFreshnessExpiresAt != nil {
			t.Fatalf("status did not expire at the authenticated deadline: %+v", status)
		}
		metrics := scrape()
		for _, expected := range []string{
			`router_verification_backends{model="gpt-oss-120b",state="configured"} 4`,
			`router_verification_backends{model="gpt-oss-120b",state="verified"} 0`,
			`router_verification_backends{model="gpt-oss-120b",state="expired"} 2`,
			`router_verification_failures_total{enclave="backend",model="gpt-oss-120b"} 2`,
			fmt.Sprintf(`router_verification_last_success_timestamp_seconds{enclave="backend",model="gpt-oss-120b"} %g`, float64(now.Unix())),
			fmt.Sprintf(`router_verification_expires_timestamp_seconds{enclave="backend",model="gpt-oss-120b"} %g`, float64(now.Add(time.Minute).Unix())),
			fmt.Sprintf(`router_verification_last_attempt_timestamp_seconds{enclave="backend",model="gpt-oss-120b"} %g`, float64(now.Add(time.Second).Unix())),
		} {
			if !strings.Contains(metrics, expected+"\n") {
				t.Fatalf("missing metric %s in:\n%s", expected, metrics)
			}
		}
		model.configuredHosts = nil
		manager.recordVerification(model, "backend", time.Now(), verificationVerify, errors.New("late failure"))
		status, _ = readVerificationStatus(t, model)
		if status.Configured != 0 || len(status.Hosts) != 0 || len(model.configuredHosts) != 0 || strings.Contains(scrape(), "enclave=") {
			t.Fatal("removed hosts remained visible or returned through an in-flight attempt")
		}
	})
}

func TestVerificationStatusConcurrentRefresh(t *testing.T) {
	const iterations = 100
	now := time.Now()
	manager := &EnclaveManager{models: &sync.Map{}}
	model := &Model{configuredHosts: map[string]verificationAttempt{"backend": {}}, Enclaves: map[string]*Enclave{"backend": {attestation: newAttestationState(verifiedAttestation{expiresAt: now.Add(time.Hour), verifiedAt: now, codeTag: "v1"})}}}
	manager.models.Store("gpt-oss-120b", model)
	registry := prometheus.NewRegistry()
	registry.MustRegister(manager.VerificationCollector())
	var workers sync.WaitGroup
	workers.Go(func() {
		for range iterations {
			model.mu.Lock()
			model.Enclaves["temporary"] = &Enclave{}
			model.configuredHosts["temporary"] = verificationAttempt{}
			model.mu.Unlock()
			manager.recordVerification(model, "backend", now, verificationVerify, errors.New("unreachable"))
			model.mu.Lock()
			delete(model.Enclaves, "temporary")
			delete(model.configuredHosts, "temporary")
			model.mu.Unlock()
		}
	})
	defer workers.Wait()
	for range iterations {
		status, _ := readVerificationStatus(t, model)
		if status.Verified != 1 || status.Configured != status.Verified+status.Expired+status.Unverified || len(status.Hosts) != status.Configured {
			t.Fatalf("inconsistent status during refresh: %+v", status)
		}
		if _, err := registry.Gather(); err != nil {
			t.Fatal(err)
		}
	}
}
