package manager

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tinfoilsh/confidential-model-router/cacheroute"
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
	m := &Model{Enclaves: make(map[string]*Enclave, len(hosts)), expectedHosts: len(hosts)}
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

func TestCacheRoutePool(t *testing.T) {
	m := newTestModel("a", "b", "c")
	m.Enclaves["b"].inflight.Add(3)

	pool := m.CacheRoutePool()
	if pool.Size != 3 || len(pool.Candidates) != 3 {
		t.Fatalf("pool = %+v, want size 3 with 3 candidates", pool)
	}
	for _, c := range pool.Candidates {
		if c.Host == "b" && c.InFlight != 3 {
			t.Fatalf("candidate b in-flight = %d, want 3", c.InFlight)
		}
	}

	// A tripped breaker leaves the stable membership.
	tripBreaker(m.Enclaves["a"])
	pool = m.CacheRoutePool()
	if pool.Size != 3 || len(pool.Candidates) != 2 {
		t.Fatalf("pool after trip = %+v, want size 3 with 2 candidates", pool)
	}
	for _, c := range pool.Candidates {
		if c.Host == "a" {
			t.Fatal("breaker-open enclave must not be a candidate")
		}
	}

	// All breakers open: degraded fallback to the full pool, mirroring
	// NextEnclave.
	tripBreaker(m.Enclaves["b"])
	tripBreaker(m.Enclaves["c"])
	if pool = m.CacheRoutePool(); len(pool.Candidates) != 3 {
		t.Fatalf("all-open pool = %+v, want fallback to all 3", pool)
	}
}

// TestCacheRoutePoolSizeIsConfigured pins that Size reflects the config, not
// the live enclave map: during a release the map is re-attested one host at a
// time, and a multi-replica pool must stay routable for measurement.
func TestCacheRoutePoolSizeIsConfigured(t *testing.T) {
	m := newTestModel("a", "b", "c", "d")
	delete(m.Enclaves, "b")
	delete(m.Enclaves, "c")
	delete(m.Enclaves, "d")

	pool := m.CacheRoutePool()
	if pool.Size != 4 {
		t.Fatalf("Size = %d, want configured 4", pool.Size)
	}
	if len(pool.Candidates) != 1 {
		t.Fatalf("candidates = %d, want the 1 live enclave", len(pool.Candidates))
	}
}

// TestCacheRouteRequest covers the tool-loop gate: a shadow-mode dispatch
// classifies and reaches the shadow's funnel via Observe, mode off and a
// missing shadow gate out.
func TestCacheRouteRequest(t *testing.T) {
	body := map[string]any{
		"cache_salt": strings.Repeat("0", 43),
		"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("s", 5000)},
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	model := newTestModel("a", "b")
	model.CacheRoute = &config.CacheRouteConfig{Mode: "shadow"}
	em := newTestManager(map[string]*Model{"tool-observed": model})
	em.cacheRouteShadow = cacheroute.NewShadow(prometheus.NewRegistry())
	defer em.cacheRouteShadow.Close()

	keyed := func() float64 {
		c, err := cacheroute.RequestsTotal.GetMetricWithLabelValues("tool-observed", string(cacheroute.OutcomeKeyed))
		if err != nil {
			t.Fatal(err)
		}
		return testutil.ToFloat64(c)
	}
	base := keyed()

	req, settings, pool, ok := em.cacheRouteRequest("tool-observed", "/v1/chat/completions", body)
	if !ok || req == nil || pool.Size != 2 {
		t.Fatalf("gate = (%v, %+v, %+v), want open with 2-replica pool", ok, req, pool)
	}
	em.cacheRouteShadow.Observe("tool-observed", req, pool, "a", settings)
	if got := keyed() - base; got != 1 {
		t.Fatalf("keyed delta = %v, want 1", got)
	}

	// Mode off gates out.
	model.CacheRoute = nil
	if _, _, _, stillOK := em.cacheRouteRequest("tool-observed", "/v1/chat/completions", body); stillOK {
		t.Fatal("mode off must gate out")
	}

	// No shadow (tests construct managers without one) gates out.
	em.cacheRouteShadow = nil
	if _, _, _, stillOK := em.cacheRouteRequest("tool-observed", "/v1/chat/completions", body); stillOK {
		t.Fatal("missing shadow must gate out")
	}
}

// TestNextEnclavePreferring pins enforcement selection: order is honored
// among breaker-closed replicas, skip and open breakers spill down the
// order, a due probe wins over the order, and exhaustion falls back to
// random selection.
func TestNextEnclavePreferring(t *testing.T) {
	m := newTestModel("a", "b", "c")
	order := []string{"b", "c", "a"}

	if got := m.NextEnclavePreferring(order, nil); got == nil || got.host != "b" {
		t.Fatalf("pick = %v, want order head b", got)
	}
	if got := m.NextEnclavePreferring(order, map[string]bool{"b": true}); got == nil || got.host != "c" {
		t.Fatalf("pick with b skipped = %v, want c", got)
	}

	tripBreaker(m.Enclaves["b"])
	if got := m.NextEnclavePreferring(order, nil); got == nil || got.host != "c" {
		t.Fatalf("pick with b open = %v, want c", got)
	}

	// Order exhausted: fall back to random among the acceptable rest.
	if got := m.NextEnclavePreferring([]string{"b"}, nil); got == nil || got.host == "b" {
		t.Fatalf("exhausted order = %v, want random fallback avoiding b", got)
	}

	// A due probe beats the cache preference.
	probe := m.Enclaves["b"]
	probe.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	if got := m.NextEnclavePreferring([]string{"a", "c"}, nil); got == nil || got.host != "b" {
		t.Fatalf("pick with due probe = %v, want probe b", got)
	}

	// Empty order behaves like NextEnclave.
	if got := m.NextEnclavePreferring(nil, map[string]bool{"a": true, "b": true}); got == nil || got.host != "c" {
		t.Fatalf("nil order = %v, want c", got)
	}
}

func TestCacheRouteSettings(t *testing.T) {
	m := newTestModel("a", "b")
	if s := m.CacheRouteSettings(); s.Mode != cacheroute.ModeOff {
		t.Fatalf("unconfigured mode = %s, want off", s.Mode)
	}

	m.CacheRoute = &config.CacheRouteConfig{Mode: "shadow", RetentionWindowMinutes: 5}
	s := m.CacheRouteSettings()
	if s.Mode != cacheroute.ModeShadow || s.Retention != 5*time.Minute {
		t.Fatalf("settings = %+v, want shadow with 5m retention", s)
	}
}

func TestEnclaveInflightTracking(t *testing.T) {
	e := newTestEnclave("a")
	release := make(chan struct{})
	e.proxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			<-release
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
		}),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "http://example/", nil)
		e.ServeHTTP(httptest.NewRecorder(), req)
	}()

	waitFor(t, func() bool { return e.inflight.Load() == 1 })
	close(release)
	<-done
	if got := e.inflight.Load(); got != 0 {
		t.Fatalf("in-flight after completion = %d, want 0", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
