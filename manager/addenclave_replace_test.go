package manager

import (
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
)

func pollingActive(m *enclaveMetrics) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancel != nil
}

// A failed fingerprint probe must not replace a registered enclave. Before
// this was enforced, every transient probe error re-attested the host and
// installed a fresh Enclave over the old one, leaking the old poller.
func TestAddEnclaveProbeFailureKeepsExistingEnclave(t *testing.T) {
	shortVerifyTimeout(t, 500*time.Millisecond)
	addr := blackholeListener(t)

	existing := newTestEnclave(addr)
	existing.tlsKeyFP = "previous-fingerprint"
	existing.metrics.setConfig(&config.OverloadConfig{MaxRequestsWaiting: 8, RetryAfterMinutes: 1})
	t.Cleanup(existing.shutdown)

	model := &Model{Enclaves: map[string]*Enclave{addr: existing}}
	em := &EnclaveManager{models: &sync.Map{}}
	em.models.Store("test-model", model)

	if err := em.addEnclave("test-model", addr, nil); err != nil {
		t.Fatalf("expected the existing enclave to be kept, got error: %v", err)
	}
	model.mu.RLock()
	got := model.Enclaves[addr]
	model.mu.RUnlock()
	if got != existing {
		t.Fatal("enclave was replaced after a failed fingerprint probe")
	}
	if !pollingActive(existing.metrics) {
		t.Fatal("existing enclave's poller was stopped")
	}
}

// Installing a replacement for a host must retire the previous enclave:
// its poller stops and its breaker is retired, so nothing keeps scraping
// the backend on behalf of an entry that is no longer routed.
func TestInstallEnclaveRetiresPrevious(t *testing.T) {
	const host = "replace-host"
	previous := newTestEnclave(host)
	previous.metrics.setConfig(&config.OverloadConfig{MaxRequestsWaiting: 8, RetryAfterMinutes: 1})
	t.Cleanup(previous.shutdown)
	if !pollingActive(previous.metrics) {
		t.Fatal("test setup: previous enclave is not polling")
	}

	replacement := newTestEnclave(host)
	t.Cleanup(replacement.shutdown)

	model := &Model{Enclaves: map[string]*Enclave{host: previous}}
	model.mu.Lock()
	model.installEnclaveLocked(host, replacement)
	model.mu.Unlock()

	if model.Enclaves[host] != replacement {
		t.Fatal("replacement was not installed")
	}
	if pollingActive(previous.metrics) {
		t.Fatal("previous enclave's poller is still running after replacement")
	}
	if !previous.cb.Retired() {
		t.Fatal("previous enclave's breaker was not retired")
	}

	// Re-installing the same enclave is a no-op and must not retire it.
	model.mu.Lock()
	model.installEnclaveLocked(host, replacement)
	model.mu.Unlock()
	if replacement.cb.Retired() {
		t.Fatal("re-installing the same enclave retired it")
	}
}
