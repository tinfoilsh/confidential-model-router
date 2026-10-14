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

func TestReservationOverloadServingAndReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shared *config.OverloadConfig
	}{
		{"model default", &config.OverloadConfig{MaxRequestsWaiting: sharedQueueLimit, ClearRequestsWaiting: sharedQueueLimit / 2, RetryAfterMinutes: sharedRetryMinutes}},
		{"no model default", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			em, model, cfg := reservationOverloadManager(t, tc.shared)
			reserved := model.Enclaves[cfg.Hostnames[0]]
			shared := model.Enclaves[cfg.Hostnames[1]]
			for _, enclave := range []*Enclave{reserved, shared} {
				observe(enclave.metrics, reservedQueueLimit)
			}
			if reject, retry, _ := reserved.ShouldReject(); !reject || retry != time.Minute {
				t.Fatalf("reserved verdict = (%v, %v), want rejection with independent default retry", reject, retry)
			}
			if trip, clear, ok := reserved.OverloadMarks(); !ok || trip != reservedQueueLimit || clear != 0 {
				t.Fatalf("reserved marks = (%d, %d, %v), want (1, 0, true)", trip, clear, ok)
			}
			if reject, _, _ := shared.ShouldReject(); reject {
				t.Fatal("shared backend inherited the reserved threshold")
			}
			primary, spill := model.ReservationPools(reservationOverloadOrg)
			if selected, _, reject, _, _ := model.SelectServing(nil, primary, spill); selected != shared || reject {
				t.Fatalf("reserved overflow selected %v with reject=%v, want eligible shared backend", selected, reject)
			}
			if tc.shared != nil {
				observe(shared.metrics, sharedQueueLimit)
				if reject, retry, _ := shared.ShouldReject(); !reject || retry != sharedRetryMinutes*time.Minute {
					t.Fatalf("shared verdict = (%v, %v), want model threshold and retry", reject, retry)
				}
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
			if reject, retry, _ := replacement.ShouldReject(); !reject || retry != time.Minute {
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
				trip, _, enabled := enclave.OverloadMarks()
				if trip != tc.want[i] || enabled != (tc.want[i] > 0) {
					t.Fatalf("backend %d marks = (%d, %v), want (%d, %v)", i, trip, enabled, tc.want[i], tc.want[i] > 0)
				}
				wantReject := tc.want[i] == reservedQueueLimit
				if reject, _, _ := enclave.ShouldReject(); reject != wantReject {
					t.Fatalf("backend %d reject=%v after reload, want %v", i, reject, wantReject)
				}
				if !enabled && (pollingActive(enclave.metrics) || enclave.metrics.overloaded.Load()) {
					t.Fatal("disabled override retained polling or overloaded state")
				}
			}
		})
	}
}
