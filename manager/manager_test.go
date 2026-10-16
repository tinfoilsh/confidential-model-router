package manager

import (
	"context"
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

const testQueueLimit = 1

func newTestModel(hosts ...string) *Model {
	m := &Model{Overload: &config.OverloadConfig{MaxRequestsWaiting: testQueueLimit}, Enclaves: make(map[string]*Enclave, len(hosts)), expectedHosts: len(hosts)}
	for _, h := range hosts {
		m.Enclaves[h] = newTestEnclave(h)
		m.Enclaves[h].metrics = newTestMetrics(h, m.Overload)
		observe(m.Enclaves[h].metrics, 0)
	}
	return m
}

func tripBreaker(e *Enclave) {
	for i := 0; i < cbFailureThreshold; i++ {
		e.cb.RecordFailure()
	}
}

func TestHasHealthyEnclave(t *testing.T) {
	healthy := newTestModel("a", "b")
	if !healthy.HasHealthyEnclave(nil, nil) {
		t.Fatal("expected healthy model with closed breakers")
	}

	down := newTestModel("a", "b")
	tripBreaker(down.Enclaves["a"])
	tripBreaker(down.Enclaves["b"])
	if down.HasHealthyEnclave(nil, nil) {
		t.Fatal("expected unhealthy model with all breakers open")
	}

	partial := newTestModel("a", "b")
	tripBreaker(partial.Enclaves["a"])
	if !partial.HasHealthyEnclave(nil, nil) {
		t.Fatal("expected healthy model when one breaker is still closed")
	}

	empty := &Model{Enclaves: map[string]*Enclave{}}
	if empty.HasHealthyEnclave(nil, nil) {
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
	// serving selection.
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

	req, settings, pool, ok := em.cacheRouteRequest(context.Background(), "tool-observed", "/v1/chat/completions", body)
	if !ok || req == nil || pool.Size != 2 {
		t.Fatalf("gate = (%v, %+v, %+v), want open with 2-replica pool", ok, req, pool)
	}
	em.cacheRouteShadow.Observe("tool-observed", req, pool, "a", settings)
	if got := keyed() - base; got != 1 {
		t.Fatalf("keyed delta = %v, want 1", got)
	}

	// Mode off gates out.
	model.CacheRoute = nil
	if _, _, _, stillOK := em.cacheRouteRequest(context.Background(), "tool-observed", "/v1/chat/completions", body); stillOK {
		t.Fatal("mode off must gate out")
	}

	// No shadow (tests construct managers without one) gates out.
	em.cacheRouteShadow = nil
	if _, _, _, stillOK := em.cacheRouteRequest(context.Background(), "tool-observed", "/v1/chat/completions", body); stillOK {
		t.Fatal("missing shadow must gate out")
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
	reserveTestDispatch(t, e)
	go func() {
		defer close(done)
		defer e.ReleaseDispatch()
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
	BackendRunningRequests.WithLabelValues(e.modelName, e.host).Set(2)
	BackendOverloaded.WithLabelValues(e.modelName, e.host).Set(1)
	CircuitBreakerState.WithLabelValues(e.modelName, e.host).Set(0)

	e.shutdown()

	// DeleteLabelValues reports false when the series is already gone.
	if BackendQueueDepth.DeleteLabelValues(e.modelName, e.host) {
		t.Fatal("queue depth series survived shutdown")
	}
	if BackendRunningRequests.DeleteLabelValues(e.modelName, e.host) {
		t.Fatal("running requests series survived shutdown")
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

func makeOverloaded(e *Enclave) {
	observe(e.metrics, 2*testQueueLimit)
}

// TestSelectServingAllOverloadedReturnsWarmest pins the exhaustion
// fallback: when every candidate is overloaded, the dispatch serves through
// overload at the order head (the key's warmest replica), not at whatever
// host the spill loop examined last.
func TestSelectServingAllOverloadedReturnsWarmest(t *testing.T) {
	m := newTestModel("a", "b", "c")
	for _, h := range []string{"a", "b", "c"} {
		makeOverloaded(m.Enclaves[h])
	}
	for range 20 {
		if got, _, _, _, _ := m.SelectServing([]string{"b", "c", "a"}, true, nil, nil, m.Overload); got == nil || got.host != "b" {
			t.Fatalf("all-overloaded pick = %v, want warmest b", got)
		} else {
			got.ReleaseDispatch()
		}
	}
}

// TestSelectServingNeverPicksDeadHost pins that a breaker-open host is
// unreachable while any closed candidate exists, even with every closed
// candidate overloaded — busy-but-alive always beats dead.
func TestSelectServingNeverPicksDeadHost(t *testing.T) {
	m := newTestModel("a", "b", "c")
	makeOverloaded(m.Enclaves["a"])
	makeOverloaded(m.Enclaves["b"])
	tripBreaker(m.Enclaves["c"])
	m.Enclaves["c"].cb.lastFailureNano.Store(time.Now().UnixNano()) // not probe-due

	for range 200 {
		got, _, _, _, _ := m.SelectServing([]string{"a", "b"}, true, nil, nil, m.Overload)
		if got != nil {
			got.ReleaseDispatch()
		}
		if got == nil || got.host == "c" {
			t.Fatalf("pick = %v, must never be dead host c", got)
		}
		if got.host != "a" {
			t.Fatalf("pick = %v, want warmest overloaded a", got)
		}
	}
}

// TestSelectServingClaimsProbes pins that internal dispatches participate
// in breaker recovery now that their outcomes are recorded.
func TestSelectServingClaimsProbes(t *testing.T) {
	m := newTestModel("a", "b")
	tripBreaker(m.Enclaves["b"])
	m.Enclaves["b"].cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano()) // probe due

	got, claim, _, _, _ := m.SelectServing([]string{"a"}, true, nil, nil, m.Overload)
	if got != nil {
		got.ReleaseDispatch()
	}
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

// TestSelectServingWithoutClaimingLeavesProbes pins the selection used
// by callers that record no breaker outcomes (MCP sessions, file
// conversion): a due probe must be left for a path that can resolve it.
func TestSelectServingWithoutClaimingLeavesProbes(t *testing.T) {
	m := newTestModel("a", "b")
	tripBreaker(m.Enclaves["b"])
	m.Enclaves["b"].cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano()) // probe due

	got, claim, _, _, _ := m.SelectServing([]string{"a"}, false, nil, nil, m.Overload)
	if got != nil {
		got.ReleaseDispatch()
	}
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
