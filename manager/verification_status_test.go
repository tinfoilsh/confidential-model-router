package manager

import (
	"context"
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
	"github.com/tinfoilsh/tinfoil-go"

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
			Reservations:    []config.ReservationConfig{{OrgIDs: []string{"private-org"}, UserIDs: []string{"private-user"}, Enclaves: []string{"backend"}}},
			configuredHosts: map[string]verificationAttempt{"backend": {}, "open": {}, "missing": {}, "harness": {}},
			Enclaves:        map[string]*Enclave{"backend": {cb: newCircuitBreaker()}, "open": {cb: newCircuitBreaker()}, "harness": {}},
		}
		model.Enclaves["backend"].client = &stubClient{verified: attestation{expiresAt: now.Add(time.Minute), verifiedAt: now, codeTag: "v1", predicate: sevSNPPredicate, tlsKeyFP: "tls-key", hpkeKey: "hpke-key"}}
		model.Enclaves["open"].client = &stubClient{verified: *testAttestation(now.Add(time.Minute), now, "v2")}
		model.Enclaves["open"].cb.storeState(cbOpen)
		manager.models.Store("gpt-oss-120b", model)
		manager.recordVerification(model, "backend", now, errors.New("first upstream error"))
		manager.recordVerification(model, "backend", now, errors.New("secret upstream error"))
		status, data := readVerificationStatus(t, model)
		if status.Configured != 4 || status.Verified != 2 || status.Healthy != 1 || status.Expired != 0 || status.Unverified != 2 || len(status.Hosts) != 4 {
			t.Fatalf("incorrect pool counts: %+v", status)
		}
		_, missingVisible := status.Hosts["missing"]
		if !missingVisible || status.Hosts["missing"].LastSuccess != nil || status.Hosts["harness"].ExpiresAt != nil || status.Hosts["backend"].LastError != verificationVerify || status.Hosts["open"].CodeTag != "v2" || status.EarliestFreshnessExpiresAt == nil || !status.EarliestFreshnessExpiresAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("incorrect authenticated state or failed-attempt details: %+v", status)
		}
		if strings.Contains(data, "private-org") || strings.Contains(data, "private-user") || strings.Contains(data, "reservations") || strings.Contains(data, "secret upstream error") {
			t.Fatalf("public status exposed private details: %s", data)
		}
		var legacy struct {
			Repo, Tag  string
			Enclaves   map[string]map[string]string
			Overload   *config.OverloadConfig
			CacheRoute *config.CacheRouteConfig `json:"cache_route"`
		}
		if err := json.Unmarshal([]byte(data), &legacy); err != nil {
			t.Fatalf("legacy decoder rejected status: %v", err)
		}
		if backend := legacy.Enclaves["backend"]; legacy.Repo != model.Repo || legacy.Tag != model.Tag || backend["predicate"] != sevSNPPredicate || backend["tls_key_fp"] != "tls-key" || backend["hpke_key"] != "hpke-key" {
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
		manager.recordVerification(model, "backend", time.Now(), nil)
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
		manager.recordVerification(model, "backend", time.Now(), errors.New("late failure"))
		status, _ = readVerificationStatus(t, model)
		if status.Configured != 0 || len(status.Hosts) != 0 || len(model.configuredHosts) != 0 || strings.Contains(scrape(), "enclave=") {
			t.Fatal("removed hosts remained visible or returned through an in-flight attempt")
		}
	})
}

// A model reports its project, and the tag and config revision of its latest
// verification. Each enclave reports its own, and one launched from a
// repository has no config revision. A model pinned to a repository alone
// reports no project or revision at all.
func TestStatusReportsRegistryProjectAndRevision(t *testing.T) {
	const (
		registryHost   = "registry.example"
		repositoryHost = "repository.example"
		runtimeTag     = "v0.14.19"
		codeTag        = "v1.0.0"
		revision       = "v3"
	)
	now := time.Now()
	fromRegistry := *testAttestation(now.Add(2*attestationRenewalWindow), now, runtimeTag)
	fromRegistry.configRevision = revision
	fromRepository := *testAttestation(now.Add(2*attestationRenewalWindow), now, codeTag)
	verifications := map[string]attestation{registryHost: fromRegistry, repositoryHost: fromRepository}
	report := func(t *testing.T, anchors config.Model, hosts ...string) (modelVerificationStatus, string) {
		t.Helper()
		em := &EnclaveManager{models: &sync.Map{}, refreshInterval: renewalTestRefresh, connect: func(_ context.Context, host string, _ tinfoil.Pin) (enclaveClient, error) {
			return &stubClient{verified: verifications[host]}, nil
		}}
		anchors.Hostnames = hosts
		em.addModel(renewalTestModel, anchors)
		model, _ := em.GetModel(renewalTestModel)
		for _, host := range hosts {
			if err := em.addEnclave(renewalTestModel, host); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(model.Enclaves[host].shutdown)
		}
		return readVerificationStatus(t, model)
	}

	for _, tc := range []struct {
		name   string
		hosts  []string
		latest attestation
	}{
		{"registry enclave verified last", []string{repositoryHost, registryHost}, fromRegistry},
		{"repository enclave verified last", []string{registryHost, repositoryHost}, fromRepository},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, data := report(t, config.Model{Repo: renewalTestRepo, Project: renewalTestProject}, tc.hosts...)
			var model struct{ Repo, Project, Tag, Revision string }
			if err := json.Unmarshal([]byte(data), &model); err != nil {
				t.Fatal(err)
			}
			if model.Repo != renewalTestRepo || model.Project != renewalTestProject || model.Tag != tc.latest.codeTag || model.Revision != tc.latest.configRevision {
				t.Fatalf("model reported as %+v, want the latest verification %+v", model, tc.latest)
			}
			registry, repository := status.Hosts[registryHost], status.Hosts[repositoryHost]
			if registry.CodeTag != runtimeTag || registry.ConfigRevision != revision || repository.CodeTag != codeTag || repository.ConfigRevision != "" {
				t.Fatalf("enclaves reported as %+v and %+v", registry, repository)
			}
			if !strings.Contains(data, fmt.Sprintf(`"project":%q`, renewalTestProject)) || !strings.Contains(data, fmt.Sprintf(`"config_revision":%q`, revision)) {
				t.Fatalf("status lacks the project or config revision: %s", data)
			}
		})
	}

	_, data := report(t, config.Model{Repo: renewalTestRepo}, repositoryHost)
	for _, field := range []string{`"project"`, `"revision"`, `"config_revision"`} {
		if strings.Contains(data, field) {
			t.Fatalf("status of a model pinned to a repository alone gained %s: %s", field, data)
		}
	}
}

func TestVerificationStatusConcurrentRefresh(t *testing.T) {
	const iterations = 100
	now := time.Now()
	manager := &EnclaveManager{models: &sync.Map{}}
	model := &Model{configuredHosts: map[string]verificationAttempt{"backend": {}}, Enclaves: map[string]*Enclave{"backend": {}}}
	model.Enclaves["backend"].client = &stubClient{verified: *testAttestation(now.Add(time.Hour), now, "v1")}
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
			manager.recordVerification(model, "backend", now, errors.New("unreachable"))
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
