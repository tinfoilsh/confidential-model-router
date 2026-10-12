package manager

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/tinfoil-go/document"
	"github.com/tinfoilsh/tinfoil-go/verify"
)

// TINFOIL_VERIFIER_TEST_CONFIG opts into read-only verification of a live pool.
func TestLiveV3Verification(t *testing.T) {
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
	verifier, err := verify.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	checkedTypes := map[string]bool{}
	for model, entry := range cfg.Models {
		for _, host := range entry.Hostnames {
			t.Run(model+"/"+host, func(t *testing.T) {
				nonce, err := document.RandomNonce()
				if err != nil {
					t.Fatal(err)
				}
				body, err := attestationFetch(host, nonce)
				if err != nil {
					t.Fatal(err)
				}
				verified, err := verifier.VerifyV3(body, nonce, entry.Repo)
				if err != nil {
					t.Fatal(err)
				}
				key, err := verified.TLSPublicKeyFP()
				if err != nil {
					t.Fatal(err)
				}
				actual, err := tlsPublicKeyFP(host)
				if err != nil {
					t.Fatal(err)
				}
				if key != actual {
					t.Fatalf("attested TLS key does not match serving certificate")
				}
				parsed, err := document.Parse(body, nonce)
				if err != nil {
					t.Fatal(err)
				}
				kind := string(verified.EnclaveMeasurement.Type) + "/" + parsed.CollateralFormat()
				t.Logf("verified %s, expires %s", kind, verified.FreshnessExpiresAt)
				if checkedTypes[kind] {
					return
				}
				checkedTypes[kind] = true
				otherNonce := append([]byte(nil), nonce...)
				otherNonce[0] ^= 1
				if _, err := verifier.VerifyV3(body, otherNonce, entry.Repo); err == nil {
					t.Fatal("accepted wrong nonce")
				}
				if _, err := verifier.VerifyV3(body, nonce, "tinfoilsh/wrong-workload"); err == nil {
					t.Fatal("accepted wrong workload")
				}
				var tampered map[string]json.RawMessage
				if err := json.Unmarshal(body, &tampered); err != nil {
					t.Fatal(err)
				}
				tampered["crypto_material"] = json.RawMessage(`"e30="`)
				badBody, err := json.Marshal(tampered)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := verifier.VerifyV3(badBody, nonce, entry.Repo); err == nil {
					t.Fatal("accepted substituted keys")
				}
			})
		}
	}
	if len(checkedTypes) == 0 {
		t.Fatal("config contained no enclaves")
	}
}

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
	verifier, err := verify.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	em := &EnclaveManager{models: &sync.Map{}, verifier: verifier, refreshInterval: time.Minute}
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
	if _, err := json.Marshal(em.Status()); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified %d configured backends; renewed %d platform types", checked, len(renewedTypes))
}
