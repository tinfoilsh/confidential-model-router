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
		if got, _ := m.NextEnclave(nil); got == nil || got.host != "b" {
			t.Fatalf("expected b, got %v", got)
		}
	}
}

func TestNextEnclave_SkipExcludesFromPreferred(t *testing.T) {
	m := newTestModel("a", "b", "c")
	got, _ := m.NextEnclave(map[string]bool{"a": true, "b": true})
	if got == nil || got.host != "c" {
		t.Fatalf("expected c, got %v", got)
	}
}

func TestNextEnclave_AllOverloadedFallsBack(t *testing.T) {
	m := newTestModel("a", "b")
	m.Enclaves["a"].metrics.overloaded.Store(true)
	m.Enclaves["b"].metrics.overloaded.Store(true)
	if got, _ := m.NextEnclave(nil); got == nil {
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

	got, claim := m.NextEnclave(map[string]bool{"probe": true})
	if got == nil || got.host != "probe" {
		t.Fatalf("expected probe, got %v", got)
	}
	if claim == nil {
		t.Fatal("probe pick must carry its claim")
	}
}

func TestNextEnclave_EmptyModel(t *testing.T) {
	m := &Model{Enclaves: map[string]*Enclave{}}
	if got, _ := m.NextEnclave(nil); got != nil {
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

	if got, _ := m.NextEnclavePreferring(order, nil); got == nil || got.host != "b" {
		t.Fatalf("pick = %v, want order head b", got)
	}
	if got, _ := m.NextEnclavePreferring(order, map[string]bool{"b": true}); got == nil || got.host != "c" {
		t.Fatalf("pick with b skipped = %v, want c", got)
	}

	// An overloaded order host is still returned: the overload flag must
	// never move a key's home — stepping aside is the caller's per-request
	// job (skip / SelectForDispatch), not the selector's.
	m.Enclaves["b"].metrics.overloaded.Store(true)
	if got, _ := m.NextEnclavePreferring(order, nil); got == nil || got.host != "b" {
		t.Fatalf("pick with b overloaded = %v, want b (no overload veto)", got)
	}
	m.Enclaves["b"].metrics.overloaded.Store(false)

	tripBreaker(m.Enclaves["b"])
	// Keep the trip fresh so a stalled test runner cannot cross the probe
	// cooldown and turn the open breaker into a served probe mid-test.
	m.Enclaves["b"].cb.lastFailureNano.Store(time.Now().UnixNano())
	if got, _ := m.NextEnclavePreferring(order, nil); got == nil || got.host != "c" {
		t.Fatalf("pick with b open = %v, want c", got)
	}

	// Order exhausted: fall back to random among the acceptable rest.
	m.Enclaves["b"].cb.lastFailureNano.Store(time.Now().UnixNano())
	if got, _ := m.NextEnclavePreferring([]string{"b"}, nil); got == nil || got.host == "b" {
		t.Fatalf("exhausted order = %v, want random fallback avoiding b", got)
	}

	// A due probe beats the cache preference.
	probe := m.Enclaves["b"]
	probe.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	if got, claim := m.NextEnclavePreferring([]string{"a", "c"}, nil); got == nil || got.host != "b" || claim == nil {
		t.Fatalf("pick with due probe = %v (claim %v), want claimed probe b", got, claim)
	}

	// Empty order behaves like NextEnclave.
	if got, _ := m.NextEnclavePreferring(nil, map[string]bool{"a": true, "b": true}); got == nil || got.host != "c" {
		t.Fatalf("nil order = %v, want c", got)
	}
}

// TestSelectForDispatch pins the internal-dispatch overload spill: an
// overloaded order head is skipped for the next-warmest host, and when every
// candidate is overloaded the dispatch still gets an enclave (internal
// callers have no 429 path).
func TestSelectForDispatch(t *testing.T) {
	m := newTestModel("a", "b", "c")
	order := []string{"b", "c", "a"}

	if got, _ := m.SelectForDispatch(order); got == nil || got.host != "b" {
		t.Fatalf("pick = %v, want order head b", got)
	}

	m.Enclaves["b"].metrics.overloaded.Store(true)
	m.Enclaves["b"].metrics.cfg = &config.OverloadConfig{MaxRequestsWaiting: 1, RetryAfterMinutes: 1}
	m.Enclaves["b"].metrics.updateLatest(5, time.Now())
	if got, _ := m.SelectForDispatch(order); got == nil || got.host != "c" {
		t.Fatalf("pick with b overloaded = %v, want spill to c", got)
	}

	for _, h := range []string{"a", "c"} {
		m.Enclaves[h].metrics.overloaded.Store(true)
		m.Enclaves[h].metrics.cfg = &config.OverloadConfig{MaxRequestsWaiting: 1, RetryAfterMinutes: 1}
		m.Enclaves[h].metrics.updateLatest(5, time.Now())
	}
	if got, _ := m.SelectForDispatch(order); got == nil {
		t.Fatal("all-overloaded dispatch must still return an enclave")
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

// TestShutdownDropsEnclaveGaugeSeries pins that removing an enclave removes
// its gauge series: a stale gauge for a departed host keeps asserting queue
// depth, overload, and breaker state until process restart, overcounting
// pool-size queries after a resize.
func TestShutdownDropsEnclaveGaugeSeries(t *testing.T) {
	e := newTestEnclave("gauge-drop-host")
	e.modelName = "gauge-drop-model"
	BackendQueueDepth.WithLabelValues(e.modelName, e.host).Set(3)
	BackendOverloaded.WithLabelValues(e.modelName, e.host).Set(1)
	CircuitBreakerState.WithLabelValues(e.modelName, e.host).Set(0)

	e.shutdown()

	// DeleteLabelValues reports false when the series is already gone.
	if BackendQueueDepth.DeleteLabelValues(e.modelName, e.host) {
		t.Fatal("queue depth series survived shutdown")
	}
	if BackendOverloaded.DeleteLabelValues(e.modelName, e.host) {
		t.Fatal("overloaded series survived shutdown")
	}
	if CircuitBreakerState.DeleteLabelValues(e.modelName, e.host) {
		t.Fatal("breaker state series survived shutdown")
	}
	if !e.cb.Retired() {
		t.Fatal("breaker not retired by shutdown")
	}
}

// TestRetiredBreakerDoesNotRepublish pins the draining-request edge: a proxy
// callback that completes after the enclave was removed must not resurrect
// the deleted breaker gauge series.
func TestRetiredBreakerDoesNotRepublish(t *testing.T) {
	const model, host = "gauge-retire-model", "gauge-retire-host"
	cb := newCircuitBreaker()

	publishBreakerState(model, host, cb)
	if !CircuitBreakerState.DeleteLabelValues(model, host) {
		t.Fatal("live breaker must publish its gauge")
	}

	cb.Retire()
	publishBreakerState(model, host, cb)
	if CircuitBreakerState.DeleteLabelValues(model, host) {
		t.Fatal("retired breaker republished the deleted gauge series")
	}
}

// TestShutdownSerializesWithBreakerPublication forces the TOCTOU ordering
// where a draining callback passes the retired check before shutdown starts.
// Shutdown must wait for that publication and delete its series afterward.
func TestShutdownSerializesWithBreakerPublication(t *testing.T) {
	const model, host = "gauge-race-model", "gauge-race-host"
	e := newTestEnclave(host)
	e.modelName = model

	publishEntered := make(chan struct{})
	allowPublish := make(chan struct{})
	publishDone := make(chan struct{})
	await := func(name string, done <-chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", name)
		}
	}
	defer func() {
		select {
		case <-allowPublish:
		default:
			close(allowPublish)
		}
	}()

	go func() {
		defer close(publishDone)
		e.cb.publishIfActive(func(state cbState) {
			close(publishEntered)
			<-allowPublish
			CircuitBreakerState.WithLabelValues(model, host).Set(float64(state))
		})
	}()
	await("publication to enter", publishEntered)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		e.shutdown()
	}()

	// Retire marks the breaker before waiting on its write barrier. A failed
	// TryRLock proves shutdown is queued behind the in-flight publication.
	waitFor(t, func() bool {
		if !e.cb.Retired() {
			return false
		}
		if e.cb.publishMu.TryRLock() {
			e.cb.publishMu.RUnlock()
			return false
		}
		return true
	})

	close(allowPublish)
	await("publication to finish", publishDone)
	await("shutdown to finish", shutdownDone)
	if CircuitBreakerState.DeleteLabelValues(model, host) {
		t.Fatal("breaker publication survived shutdown")
	}
}

// makeOverloaded flags an enclave overloaded with a fresh queue sample so
// ShouldReject fires deterministically.
func makeOverloaded(e *Enclave) {
	e.metrics.cfgMu.Lock()
	e.metrics.cfg = &config.OverloadConfig{MaxRequestsWaiting: 1, RetryAfterMinutes: 1}
	e.metrics.cfgMu.Unlock()
	e.metrics.updateLatest(5, time.Now())
	e.metrics.overloaded.Store(true)
}

// TestSelectForDispatchAllOverloadedReturnsWarmest pins the exhaustion
// fallback: when every candidate is overloaded, the dispatch serves through
// overload at the order head (the key's warmest replica), not at whatever
// host the spill loop examined last.
func TestSelectForDispatchAllOverloadedReturnsWarmest(t *testing.T) {
	m := newTestModel("a", "b", "c")
	for _, h := range []string{"a", "b", "c"} {
		makeOverloaded(m.Enclaves[h])
	}
	for range 20 {
		if got, _ := m.SelectForDispatch([]string{"b", "c", "a"}); got == nil || got.host != "b" {
			t.Fatalf("all-overloaded pick = %v, want warmest b", got)
		}
	}
}

// TestSelectForDispatchNeverPicksDeadHost pins that a breaker-open host is
// unreachable while any closed candidate exists, even with every closed
// candidate overloaded — busy-but-alive always beats dead.
func TestSelectForDispatchNeverPicksDeadHost(t *testing.T) {
	m := newTestModel("a", "b", "c")
	makeOverloaded(m.Enclaves["a"])
	makeOverloaded(m.Enclaves["b"])
	tripBreaker(m.Enclaves["c"])
	m.Enclaves["c"].cb.lastFailureNano.Store(time.Now().UnixNano()) // not probe-due

	for range 200 {
		got, _ := m.SelectForDispatch([]string{"a", "b"})
		if got == nil || got.host == "c" {
			t.Fatalf("pick = %v, must never be dead host c", got)
		}
		if got.host != "a" {
			t.Fatalf("pick = %v, want warmest overloaded a", got)
		}
	}
}

// TestSelectForDispatchClaimsProbes pins that internal dispatches participate
// in breaker recovery now that their outcomes are recorded.
func TestSelectForDispatchClaimsProbes(t *testing.T) {
	m := newTestModel("a", "b")
	tripBreaker(m.Enclaves["b"])
	m.Enclaves["b"].cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano()) // probe due

	got, claim := m.SelectForDispatch([]string{"a"})
	if got == nil || got.host != "b" {
		t.Fatalf("pick = %v, want due probe b", got)
	}
	if claim == nil {
		t.Fatal("probe pick must carry its claim")
	}
	if st := m.Enclaves["b"].cb.State(); st != cbHalfOpen {
		t.Fatalf("breaker state = %v, want half-open after probe claim", st)
	}
}

// TestSelectForDispatchWithoutClaimingLeavesProbes pins the selection used
// by callers that record no breaker outcomes (MCP sessions, file
// conversion): a due probe must be left for a path that can resolve it.
func TestSelectForDispatchWithoutClaimingLeavesProbes(t *testing.T) {
	m := newTestModel("a", "b")
	tripBreaker(m.Enclaves["b"])
	m.Enclaves["b"].cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano()) // probe due

	got, claim := m.selectForDispatch([]string{"a"}, false)
	if got == nil || got.host != "a" {
		t.Fatalf("pick = %v, want a", got)
	}
	if claim != nil {
		t.Fatal("non-claiming selection must not return a probe claim")
	}
	if st := m.Enclaves["b"].cb.State(); st != cbOpen {
		t.Fatalf("breaker state = %v, want still open (probe left unclaimed)", st)
	}
}

// TestShouldRejectNonClosedBreaker pins that overload rejection only applies
// to healthy replicas: a probe or last-resort pick must dispatch.
func TestShouldRejectNonClosedBreaker(t *testing.T) {
	e := newTestEnclave("reject-open-host")
	makeOverloaded(e)
	if reject, _, _ := e.ShouldReject(); !reject {
		t.Fatal("closed + overloaded must reject")
	}
	tripBreaker(e)
	if reject, _, _ := e.ShouldReject(); reject {
		t.Fatal("non-closed breaker must not be overload-rejected")
	}
}
