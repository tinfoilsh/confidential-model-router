package manager

import (
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
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

func TestOverloadMarks(t *testing.T) {
	var nilEnclave *Enclave
	if _, _, ok := nilEnclave.OverloadMarks(); ok {
		t.Fatal("expected ok=false for nil enclave")
	}

	e := newTestEnclave("marks-host")
	if _, _, ok := e.OverloadMarks(); ok {
		t.Fatal("expected ok=false without overload config")
	}

	e.metrics = newTestMetrics("marks-host", &config.OverloadConfig{MaxRequestsWaiting: 16, ClearRequestsWaiting: 12})
	if trip, clear, ok := e.OverloadMarks(); !ok || trip != 16 || clear != 12 {
		t.Fatalf("OverloadMarks() = (%d, %d, %v), want (16, 12, true)", trip, clear, ok)
	}

	e.metrics = newTestMetrics("marks-host", &config.OverloadConfig{MaxRequestsWaiting: 0})
	if _, _, ok := e.OverloadMarks(); ok {
		t.Fatal("expected ok=false with non-positive trip mark")
	}
}

func tripBreaker(e *Enclave) {
	for i := 0; i < cbFailureThreshold; i++ {
		e.cb.RecordFailure()
	}
}

func TestHasHealthyEnclave(t *testing.T) {
	healthy := newTestModel("a", "b")
	if !healthy.HasHealthyEnclave() {
		t.Fatal("expected healthy model with closed breakers")
	}

	down := newTestModel("a", "b")
	tripBreaker(down.Enclaves["a"])
	tripBreaker(down.Enclaves["b"])
	if down.HasHealthyEnclave() {
		t.Fatal("expected unhealthy model with all breakers open")
	}

	partial := newTestModel("a", "b")
	tripBreaker(partial.Enclaves["a"])
	if !partial.HasHealthyEnclave() {
		t.Fatal("expected healthy model when one breaker is still closed")
	}

	empty := &Model{Enclaves: map[string]*Enclave{}}
	if empty.HasHealthyEnclave() {
		t.Fatal("expected unhealthy model with no enclaves")
	}
}

func newTestManager(models map[string]*Model) *EnclaveManager {
	em := &EnclaveManager{models: &sync.Map{}}
	for name, m := range models {
		em.models.Store(name, m)
	}
	return em
}

func TestResolvePreferredModel(t *testing.T) {
	down := newTestModel("a")
	tripBreaker(down.Enclaves["a"])
	em := newTestManager(map[string]*Model{
		"glm-5-2":         down,
		"kimi-k2-6":       newTestModel("b"),
		"deepseek-v4-pro": newTestModel("c"),
	})

	// First candidate is down -> skip to the next healthy one.
	if got := em.ResolvePreferredModel([]string{"glm-5-2", "kimi-k2-6", "deepseek-v4-pro"}); got != "kimi-k2-6" {
		t.Fatalf("expected kimi-k2-6, got %q", got)
	}

	// First candidate healthy -> use it.
	if got := em.ResolvePreferredModel([]string{"kimi-k2-6", "deepseek-v4-pro"}); got != "kimi-k2-6" {
		t.Fatalf("expected kimi-k2-6, got %q", got)
	}

	// None healthy -> fall back to the first candidate.
	tripBreaker(em.mustModel(t, "kimi-k2-6").Enclaves["b"])
	tripBreaker(em.mustModel(t, "deepseek-v4-pro").Enclaves["c"])
	if got := em.ResolvePreferredModel([]string{"glm-5-2", "kimi-k2-6"}); got != "glm-5-2" {
		t.Fatalf("expected glm-5-2 fallback, got %q", got)
	}

	// Unknown model is treated as the first candidate fallback when unhealthy.
	if got := em.ResolvePreferredModel([]string{"does-not-exist", "kimi-k2-6"}); got != "does-not-exist" {
		t.Fatalf("expected does-not-exist fallback, got %q", got)
	}

	// Empty / blank-only lists resolve to "".
	if got := em.ResolvePreferredModel([]string{"", ""}); got != "" {
		t.Fatalf("expected empty resolution, got %q", got)
	}
}

func (em *EnclaveManager) mustModel(t *testing.T, name string) *Model {
	t.Helper()
	m, ok := em.GetModel(name)
	if !ok {
		t.Fatalf("model %q not found", name)
	}
	return m
}
