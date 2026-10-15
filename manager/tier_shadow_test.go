package manager

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tinfoilsh/confidential-model-router/config"
)

const (
	shadowTestModel          = "gpt-oss-120b"
	shadowTestOrg            = "example-org"
	shadowTestRunningLimit   = 4
	shadowTestLiveQueueLimit = 14
)

func shadowTestModelWithSamples(reservedWaiting, reservedRunning, sharedWaiting, sharedRunning float64) *Model {
	m := newTestModel("reserved.example", "shared.example")
	m.Overload = &config.OverloadConfig{MaxRequestsWaiting: shadowTestLiveQueueLimit}
	m.TierShadow = &config.TierShadowConfig{MaxRequestsRunning: 2 * shadowTestRunningLimit, MaxRequestsWaiting: shadowTestLiveQueueLimit}
	reservedPolicy := &config.TierShadowConfig{MaxRequestsRunning: shadowTestRunningLimit, MaxRequestsWaiting: 1}
	m.applyReservations(shadowTestModel, []config.ReservationConfig{{
		OrgIDs: []string{shadowTestOrg}, Enclaves: []string{"reserved.example"}, TierShadow: reservedPolicy,
	}}, []string{"reserved.example", "shared.example"})
	for host, counts := range map[string][2]float64{
		"reserved.example": {reservedWaiting, reservedRunning}, "shared.example": {sharedWaiting, sharedRunning},
	} {
		e := m.Enclaves[host]
		e.modelName = shadowTestModel
		e.metrics = newTestMetrics(host, m.Overload)
		observe(e.metrics, counts[0])
		e.tierShadow = &tierShadowMetrics{
			host: host, model: shadowTestModel,
			sample: tierShadowSample{waiting: counts[0], running: counts[1], hasWaiting: true, hasRunning: true, collected: time.Now()},
			queues: map[config.TierShadowConfig]bool{*m.TierShadow: false, *reservedPolicy: false},
		}
		e.tierShadow.updateQueuesLocked()
	}
	return m
}

func TestTierShadowPreservesLiveDecisions(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		waiting, running, sharedWaiting float64
		public, serveThrough, missing   bool
		actual, proposed, comparison    string
	}{
		{name: "agreement", running: 1, actual: shadowReservedPool, proposed: shadowReservedPool, comparison: shadowSame},
		{name: "earlier spill is observation only", running: shadowTestRunningLimit, actual: shadowReservedPool, proposed: shadowSharedPool, comparison: shadowDifferentPool},
		{name: "stricter queues do not reject", waiting: 1, sharedWaiting: 1, actual: shadowReservedPool, proposed: shadowReject, comparison: shadowDifferentAdmission},
		{name: "live spill remains authoritative", waiting: shadowTestLiveQueueLimit, sharedWaiting: 1, actual: shadowSharedPool, proposed: shadowReject, comparison: shadowDifferentAdmission},
		{name: "both policies reject", waiting: shadowTestLiveQueueLimit, sharedWaiting: shadowTestLiveQueueLimit, actual: shadowReject, proposed: shadowReject, comparison: shadowSame},
		{name: "internal dispatch serves through", waiting: shadowTestLiveQueueLimit, sharedWaiting: shadowTestLiveQueueLimit, serveThrough: true, actual: shadowReservedPool, proposed: shadowReservedPool, comparison: shadowSame},
		{name: "public caller uses public policy", waiting: 1, sharedWaiting: 1, public: true, actual: shadowSharedPool, proposed: shadowSharedPool, comparison: shadowSame},
		{name: "unknown samples are explicit", missing: true, actual: shadowReservedPool, proposed: shadowReservedPool, comparison: shadowUnknownMetrics},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := shadowTestModelWithSamples(tc.waiting, tc.running, tc.sharedWaiting, 0)
			org, caller := shadowTestOrg, shadowReservedPool
			if tc.public {
				org, caller = "", shadowSharedPool
			}
			if tc.missing {
				for _, e := range m.Enclaves {
					e.tierShadow.sample.collected = time.Time{}
				}
			}
			primary, spill := m.ReservationPools(org)
			order := []string{"reserved.example", "shared.example"}
			actual, claim, reject, _, _ := m.SelectServing(order, primary, spill)
			if tc.serveThrough {
				actual, claim = m.SelectForDispatchPools(order, primary, spill)
				reject = false
			}
			counter := tierShadowDecisions.WithLabelValues(shadowTestModel, caller, tc.actual, tc.proposed, tc.comparison)
			before := testutil.ToFloat64(counter)
			m.ObserveTierShadow(TierShadowObservation{Model: shadowTestModel, OrgID: org, Order: order, Actual: actual,
				Rejected: reject, Probe: claim != nil, CanProbe: true, ServeThrough: tc.serveThrough})
			if got := testutil.ToFloat64(counter) - before; got != 1 {
				t.Fatalf("expected comparison counter increment, got %v", got)
			}
			after, _, afterReject, _, _ := m.SelectServing(order, primary, spill)
			if tc.serveThrough {
				after, _ = m.SelectForDispatchPools(order, primary, spill)
				afterReject = false
			}
			if after != actual || afterReject != reject {
				t.Fatal("shadow observation changed live selection or rejection")
			}
		})
	}
}

func TestTierShadowDoesNotClaimRecoveryProbe(t *testing.T) {
	m := shadowTestModelWithSamples(0, shadowTestRunningLimit, 0, 0)
	shared := m.Enclaves["shared.example"]
	shared.cb.storeState(cbOpen)
	shared.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	primary, spill := m.ReservationPools(shadowTestOrg)
	actual, claim, rejected, _, _ := m.SelectServing([]string{"reserved.example"}, primary, spill)
	if actual != m.Enclaves["reserved.example"] || claim != nil || rejected {
		t.Fatal("live routing did not retain healthy reserved capacity")
	}
	counter := tierShadowDecisions.WithLabelValues(shadowTestModel, shadowReservedPool, shadowReservedPool, shadowHealth, shadowHealth)
	before := testutil.ToFloat64(counter)
	m.ObserveTierShadow(TierShadowObservation{Model: shadowTestModel, OrgID: shadowTestOrg, Actual: actual, CanProbe: true})
	if shared.cb.loadState() != cbOpen || shared.cb.probeGen.Load() != 0 || shared.inflight.Load() != 0 {
		t.Fatal("shadow observation changed breaker or request state")
	}
	if testutil.ToFloat64(counter)-before != 1 {
		t.Fatal("shadow did not identify the proposed recovery exception")
	}
}

func TestTierShadowPartialMetricsAreIndependentOfLiveGuard(t *testing.T) {
	var body atomic.Value
	body.Store("vllm:num_requests_waiting 0\nvllm:num_requests_running 0\n")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body.Load().(string))
	}))
	defer server.Close()
	waitingPolicy := config.TierShadowConfig{MaxRequestsWaiting: 1}
	runningPolicy := config.TierShadowConfig{MaxRequestsWaiting: 1, MaxRequestsRunning: shadowTestRunningLimit}
	s := &tierShadowMetrics{host: strings.TrimPrefix(server.URL, "https://"), model: shadowTestModel, client: server.Client(),
		queues: map[config.TierShadowConfig]bool{waitingPolicy: false, runningPolicy: false}}
	defer s.stop()
	live := newTestMetrics(s.host, &config.OverloadConfig{MaxRequestsWaiting: shadowTestLiveQueueLimit})
	observe(live, 0)
	liveWaiting, liveCollected := live.latestSample()
	s.scrape(t.Context())
	if s.tier(runningPolicy, time.Now()) != shadowPreferred {
		t.Fatal("complete sample did not classify the candidate")
	}
	body.Store("vllm:num_requests_waiting 2\n")
	s.scrape(t.Context())
	if s.tier(waitingPolicy, time.Now()) != shadowOverloaded || s.tier(runningPolicy, time.Now()) != shadowUnknown {
		t.Fatal("missing running metric froze a waiting-only candidate policy")
	}
	if waiting, collected := live.latestSample(); waiting != liveWaiting || collected != liveCollected || live.overloaded.Load() {
		t.Fatal("shadow polling affected live overload state")
	}
	if tierShadowBackendRequests.DeleteLabelValues(s.model, s.host, shadowRunning) {
		t.Fatal("missing running series was retained")
	}
}

func TestTierShadowReloadAndShutdown(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "vllm:num_requests_waiting 2\nvllm:num_requests_running 1\n")
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	m := newTestModel(host)
	e := m.Enclaves[host]
	e.modelName = shadowTestModel
	e.tierShadow = &tierShadowMetrics{host: host, model: shadowTestModel, client: server.Client()}
	m.TierShadow = &config.TierShadowConfig{MaxRequestsWaiting: 2}
	m.configureTierShadowLocked(e)
	defer e.shutdown()
	waitFor(t, func() bool { return e.tierShadow.tier(*m.TierShadow, time.Now()) == shadowOverloaded })
	worker := e.tierShadow
	done := worker.done
	m.configureTierShadowLocked(e)
	if e.tierShadow != worker || worker.done != done || worker.tier(*m.TierShadow, time.Now()) != shadowOverloaded {
		t.Fatal("unchanged configuration restarted the poller or lost queue recovery state")
	}
	m.TierShadow = nil
	m.configureTierShadowLocked(e)
	if e.tierShadow != nil {
		t.Fatal("disabling shadow retained its worker")
	}
	select {
	case <-done:
	default:
		t.Fatal("shadow worker did not stop")
	}
	if calls.Load() == 0 || tierShadowSampleTimestamp.DeleteLabelValues(shadowTestModel, host) {
		t.Fatal("shadow metrics were not collected and removed")
	}
	if e.metrics.currentConfig() != nil || e.metrics.overloaded.Load() {
		t.Fatal("shadow configuration changed the live policy")
	}
}
