//go:build localharness

package manager

import (
	"crypto/tls"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

// InstallFakeEnclaveForTest registers ts as the only enclave for modelName,
// pinned to the test server's actual certificate exactly as production pins
// to the attested key. It exists so the real request path can be exercised
// against a local upstream when attested enclaves are unreachable, and is
// compiled only under the localharness build tag.
func InstallFakeEnclaveForTest(em *EnclaveManager, modelName string, ts *httptest.Server) error {
	model, ok := em.GetModel(modelName)
	if !ok {
		return fmt.Errorf("model %s not configured", modelName)
	}
	host := strings.TrimPrefix(ts.URL, "https://")
	conn, err := tls.Dial("tcp", host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return err
	}
	fp, err := tinfoilClient.ConnectionCertFP(conn.ConnectionState())
	conn.Close()
	if err != nil {
		return err
	}
	cb := newCircuitBreaker()
	model.mu.Lock()
	for existing, e := range model.Enclaves {
		e.shutdown()
		delete(model.Enclaves, existing)
	}
	enclave := &Enclave{
		host:      host,
		modelName: modelName,
		tlsKeyFP:  fp,
		proxy:     newProxy(host, fp, modelName, em.billingCollector, cb),
		metrics:   newEnclaveMetrics(host, modelName),
		cb:        cb,
		pricing:   em.ModelPricing,
	}
	enclave.verification.Store(&tinfoilClient.VerifiedDocumentV3{FreshnessExpiresAt: time.Now().Add(time.Hour)})
	enclave.proxy.Transport = &attestationTransport{enclave: enclave, base: enclave.proxy.Transport}
	model.installEnclaveLocked(host, enclave)
	model.mu.Unlock()
	em.stateMu.Lock()
	em.lastSuccessfulUpdate = time.Now()
	em.stateMu.Unlock()
	return nil
}
