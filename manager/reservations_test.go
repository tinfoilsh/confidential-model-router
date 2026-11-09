package manager

import (
	"maps"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/tinfoilsh/confidential-model-router/config"
)

// newReservedTestModel builds a model with hosts a, b, c where c is reserved
// for org-reserved. a and b form the shared pool.
func newReservedTestModel() *Model {
	m := newTestModel("a", "b", "c")
	m.applyReservations("reservation-test-model",
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
	if primary, spill, _ := m.ReservationPools(Caller{OrgID: "org-any"}); primary != nil || spill != nil {
		t.Fatalf("pools without reservations = (%v, %v), want (nil, nil)", primary, spill)
	}

	m = newReservedTestModel()
	if !m.HasReservations() {
		t.Fatal("reserved model must report reservations")
	}

	// The reserved org gets its hosts as primary, the shared pool as spill.
	primary, spill, _ := m.ReservationPools(Caller{OrgID: "org-reserved"})
	if len(primary) != 1 || !primary["c"] {
		t.Fatalf("reserved primary = %v, want {c}", primary)
	}
	if len(spill) != 2 || !spill["a"] || !spill["b"] {
		t.Fatalf("reserved spill = %v, want {a, b}", spill)
	}

	// Everyone else gets the shared pool with no spill.
	for _, org := range []string{"", "org-other"} {
		primary, spill, _ = m.ReservationPools(Caller{OrgID: org})
		if len(primary) != 2 || !primary["a"] || !primary["b"] {
			t.Fatalf("shared primary for %q = %v, want {a, b}", org, primary)
		}
		if spill != nil {
			t.Fatalf("shared spill for %q = %v, want nil", org, spill)
		}
	}
}

// Reservations with no callers or no enclaves are dropped.
func TestReservationPoolsIgnoreDegenerateEntries(t *testing.T) {
	m := newTestModel("a", "b")
	m.applyReservations("reservation-test-model",
		[]config.ReservationConfig{
			{OrgIDs: nil, Enclaves: []string{"a"}},
			{OrgIDs: []string{""}, Enclaves: []string{"a"}},
			{OrgIDs: []string{"org-x"}, Enclaves: nil},
			{UserIDs: []string{""}, Enclaves: []string{"a"}},
			{UserIDs: []string{"user-x"}, Enclaves: nil},
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
	m.Overload = nil
	m.applyReservations("reservation-test-model",
		[]config.ReservationConfig{
			{OrgIDs: []string{"org-x"}, Enclaves: []string{"b", "typo-host"}},
			{OrgIDs: []string{"org-y"}, Enclaves: []string{"stale-host"}, Overload: &config.OverloadConfig{SoftMaxRequestsRunning: runningRequestLimit}},
		},
		[]string{"a", "b"},
	)

	primary, spill, _ := m.ReservationPools(Caller{OrgID: "org-x"})
	if len(primary) != 1 || !primary["b"] {
		t.Fatalf("primary = %v, want {b} with typo-host dropped", primary)
	}
	if len(spill) != 1 || !spill["a"] {
		t.Fatalf("spill = %v, want {a}", spill)
	}

	// org-y's only host is unknown: it must fall back to the shared pool,
	// not an empty primary.
	primary, spill, _ = m.ReservationPools(Caller{OrgID: "org-y"})
	if len(primary) != 1 || !primary["a"] || spill != nil {
		t.Fatalf("pools for org-y = (%v, %v), want shared ({a}, nil)", primary, spill)
	}
	if m.Enclaves["a"].metrics.configurePolicies(m.Overload, m.spillPoliciesLocked("a")) {
		t.Fatal("ignored reservation enabled shared-pool metrics polling")
	}
}

// One reservation entry can dedicate a pool to several orgs.
func TestReservationPoolsMultiOrg(t *testing.T) {
	m := newTestModel("a", "b", "c")
	m.applyReservations("reservation-test-model",
		[]config.ReservationConfig{{OrgIDs: []string{"org-reserved", "org-partner"}, Enclaves: []string{"b", "c"}}},
		[]string{"a", "b", "c"},
	)

	for _, org := range []string{"org-reserved", "org-partner"} {
		primary, spill, _ := m.ReservationPools(Caller{OrgID: org})
		if len(primary) != 2 || !primary["b"] || !primary["c"] {
			t.Fatalf("primary for %q = %v, want {b, c}", org, primary)
		}
		if len(spill) != 1 || !spill["a"] {
			t.Fatalf("spill for %q = %v, want {a}", org, spill)
		}
	}

	primary, spill, _ := m.ReservationPools(Caller{OrgID: "org-other"})
	if len(primary) != 1 || !primary["a"] || spill != nil {
		t.Fatalf("shared pools = (%v, %v), want ({a}, nil)", primary, spill)
	}
}

// The allowed set is a hard filter: even the degraded last-resort buckets
// never leak a host outside it.
func TestSelectionHonorsPoolFilter(t *testing.T) {
	m := newTestModel("a", "b", "c")
	allowed := map[string]bool{"c": true}

	for range 50 {
		if got, _, _, _, _ := m.SelectServing(nil, true, allowed, nil, m.Overload); got == nil || got.host != "c" {
			t.Fatalf("pick = %v, want c", got)
		} else {
			got.ReleaseDispatch()
		}
	}

	// The filter holds even for overloaded and breaker-open last resorts.
	makeOverloaded(m.Enclaves["c"])
	tripBreaker(m.Enclaves["c"])
	for range 50 {
		if got, _, _, _, _ := m.SelectServing(nil, false, allowed, nil, m.Overload); got == nil || got.host != "c" {
			t.Fatalf("degraded pick = %v, want c", got)
		} else {
			got.ReleaseDispatch()
		}
	}

	// An empty allowed set selects nothing.
	if got, _, _, _, _ := m.SelectServing(nil, true, map[string]bool{}, nil, m.Overload); got != nil {
		t.Fatalf("empty-pool pick = %v, want nil", got)
	}
}

// Cache-order hosts outside the allowed set are skipped, not served.
func TestAffinityHonorsPoolFilter(t *testing.T) {
	m := newTestModel("a", "b", "c")
	allowed := map[string]bool{"b": true, "c": true}

	if got, _, _, _, _ := m.SelectServing([]string{"a", "b"}, true, allowed, nil, m.Overload); got == nil || got.host != "b" {
		t.Fatalf("pick = %v, want b (a is outside the pool)", got)
	} else {
		got.ReleaseDispatch()
	}
}

// Reserved org: reserved host first, spill to shared on overload, 429
// verdict when both pools are exhausted.
func TestSelectServingReservedCaller(t *testing.T) {
	m := newReservedTestModel()
	primary, spill, policy := m.ReservationPools(Caller{OrgID: "org-reserved"})

	// Healthy reserved host serves.
	enclave, _, overloaded, _, _ := m.SelectServing(nil, true, primary, spill, policy)
	if enclave != nil {
		enclave.ReleaseDispatch()
	}
	if enclave == nil || enclave.host != "c" || overloaded {
		t.Fatalf("pick = (%v, overloaded=%v), want reserved c", enclave, overloaded)
	}

	// Reserved host overloaded: spill to the shared pool, no 429.
	makeOverloaded(m.Enclaves["c"])
	for range 50 {
		enclave, _, overloaded, _, _ = m.SelectServing(nil, true, primary, spill, policy)
		if enclave != nil {
			enclave.ReleaseDispatch()
		}
		if enclave == nil || enclave.host == "c" || overloaded {
			t.Fatalf("spill pick = (%v, overloaded=%v), want shared a or b", enclave, overloaded)
		}
	}

	// Both pools overloaded: overload verdict (429), not a silent serve.
	makeOverloaded(m.Enclaves["a"])
	makeOverloaded(m.Enclaves["b"])
	enclave, _, overloaded, _, _ = m.SelectServing(nil, true, primary, spill, policy)
	if enclave != nil {
		enclave.ReleaseDispatch()
	}
	if enclave == nil || !overloaded {
		t.Fatalf("exhausted pick = (%v, overloaded=%v), want overload verdict", enclave, overloaded)
	}
}

// An all-breaker-open reserved pool fails over to shared capacity instead
// of serving at a dead host.
func TestSelectServingReservedCallerSpillsFromDeadPool(t *testing.T) {
	m := newReservedTestModel()
	primary, spill, policy := m.ReservationPools(Caller{OrgID: "org-reserved"})

	tripBreaker(m.Enclaves["c"])
	for range 50 {
		enclave, claim, overloaded, _, _ := m.SelectServing(nil, true, primary, spill, policy)
		if enclave != nil {
			enclave.ReleaseDispatch()
		}
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
	primary, spill, policy := m.ReservationPools(Caller{OrgID: "org-other"})

	makeOverloaded(m.Enclaves["a"])
	makeOverloaded(m.Enclaves["b"])
	for range 50 {
		enclave, _, overloaded, _, _ := m.SelectServing(nil, true, primary, spill, policy)
		if enclave != nil {
			enclave.ReleaseDispatch()
		}
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
		enclave, _, overloaded, _, _ := m.SelectServing(nil, true, nil, nil, m.Overload)
		if enclave != nil {
			enclave.ReleaseDispatch()
		}
		if enclave == nil || enclave.host != "b" || overloaded {
			t.Fatalf("pick = (%v, overloaded=%v), want healthy b", enclave, overloaded)
		}
	}
}

func TestCacheRoutePoolIn(t *testing.T) {
	m := newReservedTestModel()
	primary, _, _ := m.ReservationPools(Caller{OrgID: "org-other"})

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

func gaugeState(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	pb := &dto.Metric{}
	if err := g.Write(pb); err != nil {
		t.Fatalf("failed to read gauge: %v", err)
	}
	return pb.GetGauge().GetValue()
}

func TestApplyReservationsPublishesPoolSize(t *testing.T) {
	const model = "pool-size-test-model"
	m := newTestModel("a", "b", "c")
	m.applyReservations(model,
		[]config.ReservationConfig{{OrgIDs: []string{"org-reserved"}, Enclaves: []string{"b", "c", "ghost"}}},
		[]string{"a", "b", "c"},
	)
	if got := gaugeState(t, ReservedPoolSize.WithLabelValues(model)); got != 2 {
		t.Errorf("reserved pool size = %v, want 2 (unknown host must not count)", got)
	}
	m.applyReservations(model, nil, nil)
	if got := gaugeState(t, ReservedPoolSize.WithLabelValues(model)); got != 0 {
		t.Errorf("cleared pool size = %v, want 0", got)
	}
}

func TestUserReservationPools(t *testing.T) {
	const user = "user_reserved"
	const org = "org_reserved"
	const sharedHost, userHost, mixedHost, orgHost = "shared", "user", "mixed", "org"
	hosts := []string{sharedHost, userHost, mixedHost, orgHost}
	model := newTestModel(hosts...)
	userPolicy := &config.OverloadConfig{MaxRequestsWaiting: reservedQueueLimit}
	orgPolicy := &config.OverloadConfig{MaxRequestsWaiting: sharedQueueLimit}
	reservations := []config.ReservationConfig{
		{UserIDs: []string{user}, Enclaves: []string{userHost}, Overload: userPolicy},
		{UserIDs: []string{user, "user_partner"}, OrgIDs: []string{"org_partner"}, Enclaves: []string{mixedHost}, Overload: userPolicy},
		{OrgIDs: []string{org}, Enclaves: []string{orgHost}, Overload: orgPolicy},
	}
	model.applyReservations("gpt-oss-120b", reservations, hosts)
	for _, tc := range []struct {
		name     string
		caller   Caller
		primary  map[string]bool
		policy   *config.OverloadConfig
		reserved bool
	}{
		{"personal union", Caller{UserID: user}, map[string]bool{userHost: true, mixedHost: true}, userPolicy, true},
		{"user precedence", Caller{UserID: user, OrgID: org}, map[string]bool{userHost: true, mixedHost: true}, userPolicy, true},
		{"organization fallback", Caller{UserID: "user_other", OrgID: org}, map[string]bool{orgHost: true}, orgPolicy, true},
		{"mixed user", Caller{UserID: "user_partner"}, map[string]bool{mixedHost: true}, userPolicy, true},
		{"mixed organization", Caller{OrgID: "org_partner"}, map[string]bool{mixedHost: true}, userPolicy, true},
		{"public", Caller{UserID: "user_other"}, map[string]bool{sharedHost: true}, model.Overload, false},
		{"anonymous", Caller{}, map[string]bool{sharedHost: true}, model.Overload, false},
		{"separate namespaces", Caller{OrgID: user, UserID: org}, map[string]bool{sharedHost: true}, model.Overload, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary, spill, policy := model.ReservationPools(tc.caller)
			if !maps.Equal(primary, tc.primary) || policy != tc.policy {
				t.Fatalf("pool = %v policy = %+v, want %v %+v", primary, policy, tc.primary, tc.policy)
			}
			if tc.reserved && !maps.Equal(spill, map[string]bool{sharedHost: true}) || !tc.reserved && spill != nil {
				t.Fatalf("unexpected spill pool: %v", spill)
			}
		})
	}
	if policies := model.spillPoliciesLocked(sharedHost); len(policies) == 0 {
		t.Fatal("shared hosts lost reserved caller policies")
	}
	model.applyReservations("gpt-oss-120b", reservations[:1], hosts)
	if !model.HasReservations() {
		t.Fatal("user-only reservation was dropped")
	}
	if policies := model.spillPoliciesLocked(sharedHost); len(policies) != 1 || policies[0] != userPolicy {
		t.Fatalf("shared host policies = %v, want user policy", policies)
	}
	model.applyReservations("gpt-oss-120b", nil, hosts)
	primary, spill, policy := model.ReservationPools(Caller{UserID: user})
	if model.HasReservations() || primary != nil || spill != nil || policy != model.Overload {
		t.Fatal("removing reservations retained a user's pool or policy")
	}
}
