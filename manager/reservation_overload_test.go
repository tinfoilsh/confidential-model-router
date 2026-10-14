package manager

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go/verify"
	"gopkg.in/yaml.v2"

	"github.com/tinfoilsh/confidential-model-router/config"
)

const (
	reservationOverloadOrg = "org-reserved"
	sharedQueueLimit       = 14
	reservedQueueLimit     = 1
	sharedRetryMinutes     = 3
	reservationConfigMode  = 0o600
)

func reservationOverloadManager(t *testing.T, sharedPolicy *config.OverloadConfig) (*EnclaveManager, *Model, config.Model) {
	t.Helper()
	keys := make(map[string]string)
	var hosts []string
	for range 2 {
		server, key := renewalServer(t, http.NotFound)
		host := strings.TrimPrefix(server.URL, "https://")
		hosts = append(hosts, host)
		keys[host] = key
	}
	em := &EnclaveManager{
		models: &sync.Map{}, refreshInterval: renewalTestRefresh,
		verifier: func(host, repo string) (*verify.Verification, error) {
			key, known := keys[host]
			if !known || repo != renewalTestRepo {
				t.Errorf("unexpected verification target: %s %s", host, repo)
			}
			return renewalVerification(key, time.Now().Add(time.Hour)), nil
		},
	}
	cfg := config.Model{
		Repo: renewalTestRepo, Hostnames: hosts, Overload: sharedPolicy,
		Reservations: []config.ReservationConfig{{
			OrgIDs: []string{reservationOverloadOrg}, Enclaves: hosts[:1],
			Overload: &config.OverloadConfig{MaxRequestsWaiting: reservedQueueLimit},
		}},
	}
	em.addModel(renewalTestModel, cfg)
	model, _ := em.GetModel(renewalTestModel)
	t.Cleanup(func() {
		for _, enclave := range model.Enclaves {
			enclave.shutdown()
		}
	})
	for _, host := range hosts {
		if err := em.addEnclave(renewalTestModel, host); err != nil {
			t.Fatal(err)
		}
	}
	return em, model, cfg
}

func TestReservationPolicySurvivesReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy *config.OverloadConfig
	}{
		{"disabled shared guard", nil},
		{"shared queue policy", &config.OverloadConfig{MaxRequestsWaiting: sharedQueueLimit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			em, model, cfg := reservationOverloadManager(t, tc.policy)
			reserved, shared := model.Enclaves[cfg.Hostnames[0]], model.Enclaves[cfg.Hostnames[1]]
			for _, enclave := range []*Enclave{reserved, shared} {
				observe(enclave.metrics, reservedQueueLimit)
			}
			primary, spill, policy := model.ReservationPools(reservationOverloadOrg)
			if selected, _, reject, retry, _ := model.SelectServing(nil, true, primary, spill, policy); selected != reserved || !reject || retry != time.Minute {
				t.Fatal("reserved queue policy must apply to both pools even without a running limit")
			}
			public, _, publicPolicy := model.ReservationPools("public-org")
			if selected, _, reject, _, _ := model.SelectServing(nil, true, public, nil, publicPolicy); selected != shared || reject {
				t.Fatal("reserved policy affected the public caller")
			}

			model.mu.Lock()
			reserved.tlsKeyFP = "previous-tls-key"
			model.mu.Unlock()
			if err := em.addEnclave(renewalTestModel, reserved.host); err != nil {
				t.Fatal(err)
			}
			replacement := model.Enclaves[reserved.host]
			if replacement == reserved || pollingActive(reserved.metrics) || !reserved.cb.Retired() {
				t.Fatal("replacement did not retire the previous enclave")
			}
			observe(replacement.metrics, reservedQueueLimit)
			if selected, _, reject, _, _ := model.SelectServing(nil, true, primary, spill, policy); selected != replacement || !reject {
				t.Fatal("replacement lost the reserved overload policy")
			}
		})
	}
}

func TestReservationOverloadReload(t *testing.T) {
	const (
		updatedQueueLimit         = 20
		updatedReservedQueueLimit = 3
	)
	sharedPolicy := &config.OverloadConfig{MaxRequestsWaiting: sharedQueueLimit, RetryAfterMinutes: sharedRetryMinutes}
	em, model, cfg := reservationOverloadManager(t, sharedPolicy)
	em.updateConfigURL = t.TempDir() + "/config.yaml"
	reserved, shared := model.Enclaves[cfg.Hostnames[0]], model.Enclaves[cfg.Hostnames[1]]
	for _, tc := range []struct {
		name        string
		modelPolicy *config.OverloadConfig
		override    *config.OverloadConfig
		pool        []string
		want        [2]int
	}{
		{"model update preserves override", &config.OverloadConfig{MaxRequestsWaiting: updatedQueueLimit}, &config.OverloadConfig{MaxRequestsWaiting: reservedQueueLimit}, cfg.Hostnames[:1], [2]int{reservedQueueLimit, updatedQueueLimit}},
		{"override update", sharedPolicy, &config.OverloadConfig{MaxRequestsWaiting: updatedReservedQueueLimit}, cfg.Hostnames[:1], [2]int{updatedReservedQueueLimit, sharedQueueLimit}},
		{"override removal", sharedPolicy, nil, cfg.Hostnames[:1], [2]int{sharedQueueLimit, sharedQueueLimit}},
		{"override restored", sharedPolicy, &config.OverloadConfig{MaxRequestsWaiting: reservedQueueLimit}, cfg.Hostnames[:1], [2]int{reservedQueueLimit, sharedQueueLimit}},
		{"explicit zero disables", sharedPolicy, &config.OverloadConfig{}, cfg.Hostnames[:1], [2]int{0, sharedQueueLimit}},
		{"host moves between pools", sharedPolicy, &config.OverloadConfig{MaxRequestsWaiting: reservedQueueLimit}, cfg.Hostnames[1:], [2]int{sharedQueueLimit, reservedQueueLimit}},
		{"reservations removed", sharedPolicy, nil, nil, [2]int{sharedQueueLimit, sharedQueueLimit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, enclave := range []*Enclave{reserved, shared} {
				observe(enclave.metrics, reservedQueueLimit)
			}
			cfg.Overload = tc.modelPolicy
			cfg.Reservations = nil
			if tc.pool != nil {
				cfg.Reservations = []config.ReservationConfig{{OrgIDs: []string{reservationOverloadOrg}, Enclaves: tc.pool, Overload: tc.override}}
			}
			data, err := yaml.Marshal(config.Config{Models: map[string]config.Model{renewalTestModel: cfg}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(em.updateConfigURL, data, reservationConfigMode); err != nil {
				t.Fatal(err)
			}
			if err := em.sync(); err != nil {
				t.Fatal(err)
			}
			for i, enclave := range []*Enclave{reserved, shared} {
				if model.Enclaves[enclave.host] != enclave {
					t.Fatal("configuration refresh replaced a stable enclave")
				}
				policy := model.overloadForHostLocked(enclave.host)
				var trip int
				if policy != nil {
					trip = policy.MaxRequestsWaiting
				}
				if trip != tc.want[i] {
					t.Fatalf("backend %d queue limit=%d, want %d", i, trip, tc.want[i])
				}
				if tier, _ := enclave.metrics.tier(policy); (tier == tierOverloaded) != (tc.want[i] == reservedQueueLimit) {
					t.Fatalf("backend %d tier=%v after reload, queue limit=%d", i, tier, tc.want[i])
				}
			}
		})
	}
}

func TestReservationTierPolicyReload(t *testing.T) {
	sharedPolicy := &config.OverloadConfig{MaxRequestsWaiting: sharedQueueLimit, MaxRequestsRunning: 2 * runningRequestLimit}
	em, model, cfg := reservationOverloadManager(t, sharedPolicy)
	em.updateConfigURL = t.TempDir() + "/config.yaml"
	reserved, shared := model.Enclaves[cfg.Hostnames[0]], model.Enclaves[cfg.Hostnames[1]]
	reservedPolicy := &config.OverloadConfig{MaxRequestsWaiting: reservedQueueLimit, MaxRequestsRunning: runningRequestLimit}
	reload := func(override *config.OverloadConfig, pool []string) {
		t.Helper()
		cfg.Reservations[0].Overload = override
		cfg.Reservations[0].Enclaves = pool
		data, err := yaml.Marshal(config.Config{Models: map[string]config.Model{renewalTestModel: cfg}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(em.updateConfigURL, data, reservationConfigMode); err != nil {
			t.Fatal(err)
		}
		if err := em.sync(); err != nil {
			t.Fatal(err)
		}
	}
	reload(reservedPolicy, cfg.Hostnames[:1])
	for _, enclave := range []*Enclave{reserved, shared} {
		observeCounts(enclave.metrics, 0, runningRequestLimit)
	}
	primary, spill, policy := model.ReservationPools(reservationOverloadOrg)
	if policy.MaxRequestsRunning != runningRequestLimit {
		t.Fatal("request did not inherit its reservation policy")
	}
	if selected, _, reject, _, _ := model.SelectServing(nil, true, primary, spill, policy); selected != reserved || reject {
		t.Fatal("shared native running limit overrode the reserved caller's policy")
	}

	reload(nil, cfg.Hostnames[:1])
	_, _, policy = model.ReservationPools(reservationOverloadOrg)
	if *policy != *sharedPolicy {
		t.Fatal("removing override did not restore the model policy")
	}
	if tier, _ := shared.metrics.tier(policy); tier != tierPreferred {
		t.Fatal("reload did not reclassify the existing sample")
	}

	cfg.Overload = nil
	reload(reservedPolicy, cfg.Hostnames[:1])
	if !pollingActive(shared.metrics) {
		t.Fatal("shared spillover must poll even without a native guard")
	}
	observeCounts(shared.metrics, reservedQueueLimit, 0)
	if tier, _ := shared.metrics.tier(reservedPolicy); tier != tierOverloaded {
		t.Fatal("shared metrics did not enforce the caller's queue policy")
	}
	reload(reservedPolicy, cfg.Hostnames[1:])
	primary, spill, policy = model.ReservationPools(reservationOverloadOrg)
	if !primary[shared.host] || !spill[reserved.host] || !pollingActive(reserved.metrics) {
		t.Fatal("moving the reservation lost spillover polling or pool membership")
	}
	reload(&config.OverloadConfig{}, cfg.Hostnames[1:])
	_, _, policy = model.ReservationPools(reservationOverloadOrg)
	if policy.Enabled() || pollingActive(shared.metrics) || pollingActive(reserved.metrics) {
		t.Fatal("empty policy did not disable tiers and unused polling")
	}
}
