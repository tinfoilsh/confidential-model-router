package manager

import (
	"testing"

	"github.com/tinfoilsh/confidential-model-router/config"
)

// newReservedTestModel builds a model with hosts a, b, c where c is reserved
// for org-reserved. a and b form the shared pool.
func newReservedTestModel() *Model {
	m := newTestModel("a", "b", "c")
	m.applyReservations(
		[]config.ReservationConfig{{OrgIDs: []string{"org-reserved"}, Enclaves: []string{"c"}}},
		[]string{"a", "b", "c"},
	)
	return m
}

func TestReservationPools(t *testing.T) {
	m := newTestModel("a", "b")
	if m.HasReservations() {
		t.Fatal("model without reservations must report none")
	}
	if primary, spill := m.ReservationPools("org-any"); primary != nil || spill != nil {
		t.Fatalf("pools without reservations = (%v, %v), want (nil, nil)", primary, spill)
	}

	m = newReservedTestModel()
	if !m.HasReservations() {
		t.Fatal("reserved model must report reservations")
	}

	// The reserved org gets its hosts as primary, the shared pool as spill.
	primary, spill := m.ReservationPools("org-reserved")
	if len(primary) != 1 || !primary["c"] {
		t.Fatalf("reserved primary = %v, want {c}", primary)
	}
	if len(spill) != 2 || !spill["a"] || !spill["b"] {
		t.Fatalf("reserved spill = %v, want {a, b}", spill)
	}

	// Everyone else gets the shared pool with no spill.
	for _, org := range []string{"", "org-other"} {
		primary, spill = m.ReservationPools(org)
		if len(primary) != 2 || !primary["a"] || !primary["b"] {
			t.Fatalf("shared primary for %q = %v, want {a, b}", org, primary)
		}
		if spill != nil {
			t.Fatalf("shared spill for %q = %v, want nil", org, spill)
		}
	}
}

// Reservations with no orgs or no enclaves are dropped.
func TestReservationPoolsIgnoreDegenerateEntries(t *testing.T) {
	m := newTestModel("a", "b")
	m.applyReservations(
		[]config.ReservationConfig{
			{OrgIDs: nil, Enclaves: []string{"a"}},
			{OrgIDs: []string{""}, Enclaves: []string{"a"}},
			{OrgIDs: []string{"org-x"}, Enclaves: nil},
		},
		[]string{"a", "b"},
	)
	if m.HasReservations() {
		t.Fatal("degenerate reservations must leave the model unreserved")
	}
}

// Reserved hosts missing from the configured enclave list are dropped, and
// an org left with no valid hosts is treated as unreserved rather than
// pinned to an empty pool.
func TestReservationPoolsDropUnknownHosts(t *testing.T) {
	m := newTestModel("a", "b")
	m.applyReservations(
		[]config.ReservationConfig{
			{OrgIDs: []string{"org-x"}, Enclaves: []string{"b", "typo-host"}},
			{OrgIDs: []string{"org-y"}, Enclaves: []string{"stale-host"}},
		},
		[]string{"a", "b"},
	)

	primary, spill := m.ReservationPools("org-x")
	if len(primary) != 1 || !primary["b"] {
		t.Fatalf("primary = %v, want {b} with typo-host dropped", primary)
	}
	if len(spill) != 1 || !spill["a"] {
		t.Fatalf("spill = %v, want {a}", spill)
	}

	// org-y's only host is unknown: it must fall back to the shared pool,
	// not an empty primary.
	primary, spill = m.ReservationPools("org-y")
	if len(primary) != 1 || !primary["a"] || spill != nil {
		t.Fatalf("pools for org-y = (%v, %v), want shared ({a}, nil)", primary, spill)
	}
}

// One reservation entry can dedicate a pool to several orgs.
func TestReservationPoolsMultiOrg(t *testing.T) {
	m := newTestModel("a", "b", "c")
	m.applyReservations(
		[]config.ReservationConfig{{OrgIDs: []string{"org-reserved", "org-partner"}, Enclaves: []string{"b", "c"}}},
		[]string{"a", "b", "c"},
	)

	for _, org := range []string{"org-reserved", "org-partner"} {
		primary, spill := m.ReservationPools(org)
		if len(primary) != 2 || !primary["b"] || !primary["c"] {
			t.Fatalf("primary for %q = %v, want {b, c}", org, primary)
		}
		if len(spill) != 1 || !spill["a"] {
			t.Fatalf("spill for %q = %v, want {a}", org, spill)
		}
	}

	primary, spill := m.ReservationPools("org-other")
	if len(primary) != 1 || !primary["a"] || spill != nil {
		t.Fatalf("shared pools = (%v, %v), want ({a}, nil)", primary, spill)
	}
}

// The allowed set is a hard filter: even the degraded last-resort buckets
// never leak a host outside it.
func TestNextEnclaveHonorsAllowedFilter(t *testing.T) {
	m := newTestModel("a", "b", "c")
	allowed := map[string]bool{"c": true}

	for range 50 {
		if got, _ := m.nextEnclave(nil, true, allowed); got == nil || got.host != "c" {
			t.Fatalf("pick = %v, want c", got)
		}
	}

	// The filter holds even for overloaded and breaker-open last resorts.
	makeOverloaded(m.Enclaves["c"])
	tripBreaker(m.Enclaves["c"])
	for range 50 {
		if got, _ := m.nextEnclave(nil, false, allowed); got == nil || got.host != "c" {
			t.Fatalf("degraded pick = %v, want c", got)
		}
	}

	// An empty allowed set selects nothing.
	if got, _ := m.nextEnclave(nil, true, map[string]bool{}); got != nil {
		t.Fatalf("empty-pool pick = %v, want nil", got)
	}
}

// Cache-order hosts outside the allowed set are skipped, not served.
func TestNextEnclavePreferringHonorsAllowedFilter(t *testing.T) {
	m := newTestModel("a", "b", "c")
	allowed := map[string]bool{"b": true, "c": true}

	if got, _ := m.nextEnclavePreferring([]string{"a", "b"}, nil, true, allowed); got == nil || got.host != "b" {
		t.Fatalf("pick = %v, want b (a is outside the pool)", got)
	}
}

// Reserved org: reserved host first, spill to shared on overload, 429
// verdict when both pools are exhausted.
func TestSelectServingReservedCaller(t *testing.T) {
	m := newReservedTestModel()
	primary, spill := m.ReservationPools("org-reserved")

	// Healthy reserved host serves.
	enclave, _, overloaded, _, _ := m.SelectServing(nil, primary, spill)
	if enclave == nil || enclave.host != "c" || overloaded {
		t.Fatalf("pick = (%v, overloaded=%v), want reserved c", enclave, overloaded)
	}

	// Reserved host overloaded: spill to the shared pool, no 429.
	makeOverloaded(m.Enclaves["c"])
	for range 50 {
		enclave, _, overloaded, _, _ = m.SelectServing(nil, primary, spill)
		if enclave == nil || enclave.host == "c" || overloaded {
			t.Fatalf("spill pick = (%v, overloaded=%v), want shared a or b", enclave, overloaded)
		}
	}

	// Both pools overloaded: overload verdict (429), not a silent serve.
	makeOverloaded(m.Enclaves["a"])
	makeOverloaded(m.Enclaves["b"])
	enclave, _, overloaded, _, _ = m.SelectServing(nil, primary, spill)
	if enclave == nil || !overloaded {
		t.Fatalf("exhausted pick = (%v, overloaded=%v), want overload verdict", enclave, overloaded)
	}
}

// An all-breaker-open reserved pool fails over to shared capacity instead
// of serving at a dead host.
func TestSelectServingReservedCallerSpillsFromDeadPool(t *testing.T) {
	m := newReservedTestModel()
	primary, spill := m.ReservationPools("org-reserved")

	tripBreaker(m.Enclaves["c"])
	for range 50 {
		enclave, claim, overloaded, _, _ := m.SelectServing(nil, primary, spill)
		// A due recovery probe for c is fine; a plain pick must spill.
		if claim != nil {
			continue
		}
		if enclave == nil || enclave.host == "c" || overloaded {
			t.Fatalf("pick with dead reserved pool = (%v, overloaded=%v), want shared a or b", enclave, overloaded)
		}
	}
}

// Exclusivity: shared traffic 429s on an exhausted shared pool rather than
// touching the reserved host.
func TestSelectServingSharedCallerNeverUsesReservedHost(t *testing.T) {
	m := newReservedTestModel()
	primary, spill := m.ReservationPools("org-other")

	makeOverloaded(m.Enclaves["a"])
	makeOverloaded(m.Enclaves["b"])
	for range 50 {
		enclave, _, overloaded, _, _ := m.SelectServing(nil, primary, spill)
		if enclave == nil || enclave.host == "c" {
			t.Fatalf("shared pick = %v, must never be reserved c", enclave)
		}
		if !overloaded {
			t.Fatalf("shared pick with exhausted shared pool must carry the overload verdict")
		}
	}
}

// Nil pools behave like the pre-reservation loop over all enclaves.
func TestSelectServingNoReservations(t *testing.T) {
	m := newTestModel("a", "b")
	makeOverloaded(m.Enclaves["a"])

	for range 50 {
		enclave, _, overloaded, _, _ := m.SelectServing(nil, nil, nil)
		if enclave == nil || enclave.host != "b" || overloaded {
			t.Fatalf("pick = (%v, overloaded=%v), want healthy b", enclave, overloaded)
		}
	}
}

// No-429 dispatch path: spill on primary overload, serve through overload
// at the primary when both pools are exhausted.
func TestSelectForDispatchPools(t *testing.T) {
	m := newReservedTestModel()
	primary, spill := m.ReservationPools("org-reserved")

	if got, _ := m.SelectForDispatchPools(nil, primary, spill); got == nil || got.host != "c" {
		t.Fatalf("pick = %v, want reserved c", got)
	}

	makeOverloaded(m.Enclaves["c"])
	for range 50 {
		if got, _ := m.SelectForDispatchPools(nil, primary, spill); got == nil || got.host == "c" {
			t.Fatalf("spill pick = %v, want shared a or b", got)
		}
	}

	makeOverloaded(m.Enclaves["a"])
	makeOverloaded(m.Enclaves["b"])
	for range 50 {
		if got, _ := m.SelectForDispatchPools(nil, primary, spill); got == nil || got.host != "c" {
			t.Fatalf("exhausted pick = %v, want serve-through at reserved c", got)
		}
	}
}

func TestCacheRoutePoolIn(t *testing.T) {
	m := newReservedTestModel()
	primary, _ := m.ReservationPools("org-other")

	pool := m.CacheRoutePoolIn(primary)
	if pool.Size != 2 || len(pool.Candidates) != 2 {
		t.Fatalf("pool = %+v, want size 2 with shared candidates a, b", pool)
	}
	for _, c := range pool.Candidates {
		if c.Host == "c" {
			t.Fatalf("shared cache-route pool must not contain reserved c: %+v", pool)
		}
	}

	// All allowed breakers open: the fallback stays inside the filter.
	tripBreaker(m.Enclaves["a"])
	tripBreaker(m.Enclaves["b"])
	pool = m.CacheRoutePoolIn(primary)
	if len(pool.Candidates) != 2 {
		t.Fatalf("fallback pool = %+v, want a and b", pool)
	}
	for _, c := range pool.Candidates {
		if c.Host == "c" {
			t.Fatalf("fallback must not leak reserved c: %+v", pool)
		}
	}

	// nil filter preserves the unrestricted behavior.
	if pool := m.CacheRoutePoolIn(nil); pool.Size != 3 {
		t.Fatalf("unrestricted pool = %+v, want size 3", pool)
	}
}
