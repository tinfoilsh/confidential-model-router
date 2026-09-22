//go:build localharness

package manager

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/cacheroute"
	"github.com/tinfoilsh/confidential-model-router/config"
)

// NewAdmissionManagerForTest uses the real model configuration and dispatch
// without starting remote attestation or billing workers. Never built into releases.
func NewAdmissionManagerForTest(data []byte, controlPlaneURL string) (*EnclaveManager, error) {
	cfg, err := config.FromBytes(data)
	if err != nil {
		return nil, err
	}
	em := &EnclaveManager{
		models:                    &sync.Map{},
		controlPlaneURL:           controlPlaneURL,
		usageContextSecret:        "test-context-secret",
		inferenceDelegationSecret: "test-delegation-secret",
		delegationHTTPClient:      &http.Client{},
		cacheRouteShadow:          cacheroute.NewShadow(prometheus.NewRegistry()),
	}
	for name, model := range cfg.Models {
		em.addModel(name, model)
	}
	return em, nil
}

// EnableBillingForTest attaches the real collector to internal dispatch tests;
// its returned stop function flushes pending reports before assertions.
func EnableBillingForTest(em *EnclaveManager) func() {
	em.billingCollector = billing.NewCollector(em.controlPlaneURL, "router-test", "test-secret")
	return em.billingCollector.Stop
}

// ConfigureAdmissionModelForTest publishes deterministic catalog, reservation,
// and overload state for enclaves installed by InstallFakeEnclaveForTest.
func ConfigureAdmissionModelForTest(em *EnclaveManager, name, org string, overloaded bool) error {
	model, ok := em.GetModel(name)
	if !ok {
		return fmt.Errorf("model %s not configured", name)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	hosts := make([]string, 0, len(model.Enclaves))
	for host, enclave := range model.Enclaves {
		hosts = append(hosts, host)
		if overloaded {
			enclave.metrics.cfgMu.Lock()
			enclave.metrics.cfg = &config.OverloadConfig{MaxRequestsWaiting: 1, RetryAfterMinutes: 1}
			enclave.metrics.cfgMu.Unlock()
			enclave.metrics.updateLatest(2, time.Now())
			enclave.metrics.evaluateThresholds(2)
		} else {
			enclave.updateOverloadConfig(nil)
		}
	}
	if org != "" {
		model.applyReservations(name, []config.ReservationConfig{{OrgIDs: []string{org}, Enclaves: hosts}}, hosts)
	}
	scores := map[string]ModelIntelligence{name: {Scores: map[string]int{"off": 50}}}
	em.modelIntelligence.Store(&scores)
	return nil
}
