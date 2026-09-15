package manager

import (
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
)

// Opt-in, read-only qualification of the actual router admission and transport
// path. No inference traffic, credentials, or deployment changes are needed.
func TestLiveV3Backends(t *testing.T) {
	path := os.Getenv("TINFOIL_V3_AUDIT_CONFIG")
	if path == "" {
		t.Skip("set TINFOIL_V3_AUDIT_CONFIG to a model configuration for live qualification")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	em := &EnclaveManager{models: &sync.Map{}}
	t.Cleanup(em.Shutdown)
	for name, model := range cfg.Models {
		em.addModel(name, model)
	}
	em.refreshAttestations()
	for _, failure := range em.errors {
		t.Error(failure)
	}
	checked := 0
	for name, expected := range cfg.Models {
		model, _ := em.GetModel(name)
		for _, host := range expected.Hostnames {
			checked++
			t.Run(host, func(t *testing.T) {
				enclave := model.Enclaves[host]
				if enclave == nil || !enclave.attestationValid() {
					t.Fatal("endpoint did not pass router V3 admission")
				}
				key, err := tlsPublicKeyFP(host)
				if err != nil {
					t.Fatal(err)
				}
				if key != enclave.tlsKeyFP {
					t.Fatal("TLS probe did not match the admitted key")
				}
				httpClient := &http.Client{Transport: enclave.attestedTransport(), Timeout: 20 * time.Second,
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				defer httpClient.CloseIdleConnections()
				resp, err := httpClient.Get("https://" + host + "/.well-known/tinfoil-containers")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("pinned service GET: HTTP %d", resp.StatusCode)
				}
				verified := enclave.verification.Load()
				t.Logf("V3 %s %s; freshness expires %s", expected.Repo, verified.CodeTag, verified.FreshnessExpiresAt.UTC().Format(time.RFC3339))
			})
		}
	}
	if checked == 0 {
		t.Fatal("configuration has no endpoints to qualify")
	}
	t.Logf("qualified %d configured endpoints through router V3 admission and pinned transport", checked)
}
