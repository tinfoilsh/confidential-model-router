package manager

import (
	"testing"
	"time"
)

func newTestEnclave(host string) *Enclave {
	return &Enclave{
		host:    host,
		cb:      newCircuitBreaker(),
		metrics: newEnclaveMetrics(host, "test-model"),
	}
}

func newTestModel(hosts ...string) *Model {
	m := &Model{Enclaves: make(map[string]*Enclave, len(hosts))}
	for _, h := range hosts {
		m.Enclaves[h] = newTestEnclave(h)
	}
	return m
}

func TestNextEnclave_PrefersNonOverloaded(t *testing.T) {
	m := newTestModel("a", "b")
	m.Enclaves["a"].metrics.overloaded.Store(true)

	for i := 0; i < 50; i++ {
		if got := m.NextEnclave(nil); got == nil || got.host != "b" {
			t.Fatalf("expected b, got %v", got)
		}
	}
}

func TestNextEnclave_SkipExcludesFromPreferred(t *testing.T) {
	m := newTestModel("a", "b", "c")
	got := m.NextEnclave(map[string]bool{"a": true, "b": true})
	if got == nil || got.host != "c" {
		t.Fatalf("expected c, got %v", got)
	}
}

func TestNextEnclave_AllOverloadedFallsBack(t *testing.T) {
	m := newTestModel("a", "b")
	m.Enclaves["a"].metrics.overloaded.Store(true)
	m.Enclaves["b"].metrics.overloaded.Store(true)
	if got := m.NextEnclave(nil); got == nil {
		t.Fatal("expected fallback pick, got nil")
	}
}

func TestNextEnclave_ProbeOverridesSkip(t *testing.T) {
	m := newTestModel("probe", "other")
	probe := m.Enclaves["probe"]
	for i := 0; i < cbFailureThreshold; i++ {
		probe.cb.RecordFailure()
	}
	probe.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())

	if got := m.NextEnclave(map[string]bool{"probe": true}); got == nil || got.host != "probe" {
		t.Fatalf("expected probe, got %v", got)
	}
}

func TestNextEnclave_EmptyModel(t *testing.T) {
	m := &Model{Enclaves: map[string]*Enclave{}}
	if got := m.NextEnclave(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}
