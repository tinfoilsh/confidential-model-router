//go:build localharness

package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// fakeEnclaveClient reaches a test server that no attestation covers.
type fakeEnclaveClient struct {
	http.RoundTripper
}

func (fakeEnclaveClient) current() attestation { return attestation{} }

func (fakeEnclaveClient) reverify(context.Context) (attestation, error) {
	return attestation{}, nil
}

// InstallFakeEnclaveForTest registers ts as the only enclave for modelName,
// trusting only the test server's certificate. It exists so the real request path can be exercised
// against a local upstream when attested enclaves are unreachable, and is
// compiled only under the localharness build tag.
func InstallFakeEnclaveForTest(em *EnclaveManager, modelName string, ts *httptest.Server) error {
	model, ok := em.GetModel(modelName)
	if !ok {
		return fmt.Errorf("model %s not configured", modelName)
	}
	host := strings.TrimPrefix(ts.URL, "https://")
	client := fakeEnclaveClient{ts.Client().Transport}
	cb := newCircuitBreaker()
	model.mu.Lock()
	for existing, e := range model.Enclaves {
		e.shutdown()
		delete(model.Enclaves, existing)
	}
	model.installEnclaveLocked(host, &Enclave{
		host:      host,
		modelName: modelName,
		client:    client,
		proxy:     newProxy(host, modelName, client, em.billingCollector, cb),
		metrics:   newEnclaveMetrics(host, modelName),
		cb:        cb,
		pricing:   em.ModelPricing,
	})
	model.mu.Unlock()
	em.stateMu.Lock()
	em.lastSuccessfulUpdate = time.Now()
	em.stateMu.Unlock()
	return nil
}
