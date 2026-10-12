package manager

import (
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
)

// TINFOIL_VERIFIER_TEST_CONFIG opts into read-only verification of a live pool.
func TestLiveV3ManagerRenewal(t *testing.T) {
	path := os.Getenv("TINFOIL_VERIFIER_TEST_CONFIG")
	if path == "" {
		t.Skip("set TINFOIL_VERIFIER_TEST_CONFIG to a live router config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	em := &EnclaveManager{models: &sync.Map{}, verifier: verifyEnclave, refreshInterval: time.Minute}
	t.Cleanup(em.Shutdown)
	renewedTypes := make(map[string]bool)
	checked := 0
	for name, entry := range cfg.Models {
		em.addModel(name, entry)
		model, _ := em.GetModel(name)
		for _, host := range entry.Hostnames {
			if err := em.addEnclave(name, host); err != nil {
				t.Fatalf("%s/%s: %v", name, host, err)
			}
			checked++
			backend := model.Enclaves[host]
			actualKey, err := tlsPublicKeyFP(host)
			if err != nil || actualKey != backend.tlsKeyFP {
				t.Fatalf("%s serving TLS key does not match its attestation: %v", host, err)
			}
			kind := string(backend.predicate)
			if renewedTypes[kind] {
				continue
			}
			snapshot := *backend.attestation.load()
			snapshot.verifiedAt = time.Now().Add(-attestationRefreshInterval)
			backend.attestation.current.Store(&snapshot)
			transport := newAttestedTransport(backend.tlsKeyFP, backend.attestation)
			if err := em.addEnclave(name, host); err != nil {
				t.Fatalf("renew %s: %v", host, err)
			}
			if model.Enclaves[host] != backend || !backend.attestation.load().verifiedAt.After(snapshot.verifiedAt) {
				t.Fatalf("%s did not renew in place", host)
			}
			client := &http.Client{Transport: transport, Timeout: enclaveVerifyTimeout}
			response, err := client.Get("https://" + host + "/.well-known/tinfoil-containers")
			transport.CloseIdleConnections()
			if err != nil {
				t.Fatalf("request after renewal for %s: %v", host, err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("request after renewal for %s: HTTP %d", host, response.StatusCode)
			}
			renewedTypes[kind] = true
			t.Logf("renewed %s on %s; existing transport request passed", kind, host)
		}
		model.mu.RLock()
		status := model.verificationStatusLocked(time.Now())
		model.mu.RUnlock()
		if status.Verified != status.Configured || status.Healthy != status.Configured {
			t.Fatalf("%s verification coverage: %+v", name, status)
		}
	}
	if checked == 0 {
		t.Fatal("config contained no enclaves")
	}
	t.Logf("verified %d configured backends; renewed %d platform types", checked, len(renewedTypes))
}
