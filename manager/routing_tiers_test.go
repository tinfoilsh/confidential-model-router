package manager

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
)

func TestTierRequiresWaitingAndRunningSamples(t *testing.T) {
	policy := &config.OverloadConfig{SoftMaxRequestsRunning: runningRequestLimit}
	m := newTestMetrics("sample.example", policy)
	for _, tc := range []struct {
		counts requestCounts
		want   routingTier
	}{
		{requestCounts{}, tierUnknown},
		{requestCounts{hasRunning: true}, tierUnknown},
		{requestCounts{hasWaiting: true}, tierUnknown},
		{requestCounts{hasWaiting: true, hasRunning: true}, tierPreferred},
		{requestCounts{waiting: reservedQueueLimit, hasWaiting: true, hasRunning: true}, tierBusy},
	} {
		m.updateLatest(tc.counts, time.Now())
		if tier, _ := m.tier(policy, 0); tier != tc.want {
			t.Fatalf("counts=%+v tier=%v, want %v", tc.counts, tier, tc.want)
		}
	}
}

// TestLocalInflightSpillsBurstInsidePollInterval pins the bug where a burst
// that lands within one /metrics poll interval is fully admitted to the
// reserved pool because the polled running count has not caught up yet.
func TestLocalInflightSpillsBurstInsidePollInterval(t *testing.T) {
	m := newTieredTestModel()
	primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
	order := []string{"warm", "sibling", "shared"}
	// Every pool reports idle through its sample; only the dispatch
	// counts show the burst, as during one poll interval.
	for host := range m.Enclaves {
		observeCounts(m.Enclaves[host].metrics, 0, 0)
	}
	for range runningRequestLimit {
		m.Enclaves["warm"].inflight.Add(1)
		m.Enclaves["sibling"].inflight.Add(1)
	}
	if selected, _, _, _, _ := m.SelectServing(order, true, primary, spill, policy); selected.host != "shared" {
		t.Fatal("local in-flight at the soft max did not spill to shared tier zero")
	} else {
		selected.ReleaseDispatch()
	}
	if tier, _ := m.Enclaves["warm"].metrics.tier(policy, runningRequestLimit); tier != tierBusy {
		t.Fatalf("at-cap local in-flight with idle sample tier=%v, want %v", tier, tierBusy)
	}
}

// TestLocalInflightKeepsSpillingWithoutSamples pins that dead /metrics
// polling no longer pins the caller to its first pool once the local
// dispatch count reaches the soft max.
func TestLocalInflightKeepsSpillingWithoutSamples(t *testing.T) {
	m := newTieredTestModel()
	primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
	order := []string{"warm", "sibling", "shared"}
	for host := range primary {
		m.Enclaves[host].metrics.updateLatest(requestCounts{}, time.Time{})
	}
	for range runningRequestLimit {
		m.Enclaves["warm"].inflight.Add(1)
		m.Enclaves["sibling"].inflight.Add(1)
	}
	if selected, _, _, _, _ := m.SelectServing(order, true, primary, spill, policy); selected.host != "shared" {
		t.Fatal("missing samples kept the at-cap reserved pool preferred")
	} else {
		selected.ReleaseDispatch()
	}
	// With every usable pool at its local cap and no samples, the usual
	// tier-2 overload rejection applies instead of unbounded queueing.
	m.Enclaves["shared"].metrics.updateLatest(requestCounts{}, time.Time{})
	for range 2 * runningRequestLimit {
		m.Enclaves["shared"].inflight.Add(1)
	}
	if selected, _, reject, _, _ := m.SelectServing(order, true, primary, spill, policy); selected == nil || !reject {
		t.Fatal("all pools at local cap with missing samples must reject as overloaded")
	} else {
		selected.ReleaseDispatch()
	}
}

func TestSpilloverScrapeWithoutNativeGuard(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %d\n%s 0\n", waitingMetricName, reservedQueueLimit, runningMetricName)
	}))
	defer server.Close()
	policy := &config.OverloadConfig{SoftMaxRequestsRunning: runningRequestLimit, MaxRequestsWaiting: reservedQueueLimit}
	m := newTestMetrics(strings.TrimPrefix(server.URL, "https://"), nil)
	m.client = server.Client()
	m.setPolicies(nil, []*config.OverloadConfig{policy})
	defer m.shutdown()
	m.scrape(t.Context())
	if tier, _ := m.tier(policy, 0); tier != tierOverloaded {
		t.Fatal("shared scrape did not classify load for its spillover caller")
	}
	if tier, _ := m.tier(nil, 0); tier != tierPreferred {
		t.Fatal("spillover policy enabled the disabled native guard")
	}
}

func newTieredTestModel() *Model {
	m := newTestModel("warm", "sibling", "shared", "other-reserved")
	m.Overload = &config.OverloadConfig{SoftMaxRequestsRunning: 2 * runningRequestLimit, MaxRequestsWaiting: sharedQueueLimit}
	reserved := &config.OverloadConfig{SoftMaxRequestsRunning: runningRequestLimit, MaxRequestsWaiting: reservedQueueLimit}
	m.applyReservations("gpt-oss-120b", []config.ReservationConfig{
		{OrgIDs: []string{reservationOverloadOrg}, Enclaves: []string{"warm", "sibling"}, Overload: reserved},
		{OrgIDs: []string{"other-org"}, Enclaves: []string{"other-reserved"}},
	}, []string{"warm", "sibling", "shared", "other-reserved"})
	for host, enclave := range m.Enclaves {
		enclave.metrics = newTestMetrics(host, m.overloadForHostLocked(host))
		observeCounts(enclave.metrics, 0, 0)
	}
	return m
}

func TestTieredSelection(t *testing.T) {
	type load struct{ waiting, running float64 }
	preferred := load{running: runningRequestLimit - 1}
	busy := load{running: runningRequestLimit}
	queued := load{waiting: reservedQueueLimit}
	for _, tc := range []struct {
		name                  string
		warm, sibling, shared load
		public                bool
		unknown               []string
		want                  string
		reject                bool
	}{
		{name: "affinity within reserved tier zero", warm: preferred, sibling: preferred, shared: preferred, want: "warm"},
		{name: "tier zero beats warm tier one", warm: busy, sibling: preferred, shared: preferred, want: "sibling"},
		{name: "shared tier zero beats reserved tier one", warm: busy, sibling: busy, shared: preferred, want: "shared"},
		{name: "reserved tier one beats shared tier one", warm: busy, sibling: busy, shared: busy, want: "warm"},
		{name: "shared uses reserved running limit", warm: busy, sibling: busy, shared: load{running: 2*runningRequestLimit - 1}, want: "warm"},
		{name: "shared tier one beats reserved tier two", warm: queued, sibling: queued, shared: busy, want: "shared"},
		{name: "all tier two reject", warm: queued, sibling: queued, shared: queued, want: "warm", reject: true},
		{name: "reserved waiting policy follows spillover", warm: queued, sibling: queued, shared: load{waiting: sharedQueueLimit - 1}, want: "warm", reject: true},
		{name: "public uses shared running limit", public: true, shared: load{running: 2*runningRequestLimit - 1}, want: "shared"},
		{name: "public may serve at running limit", public: true, shared: load{running: 2 * runningRequestLimit}, want: "shared"},
		{name: "public may serve below queue limit", public: true, shared: load{waiting: sharedQueueLimit - 1}, want: "shared"},
		{name: "public cannot escape queue limit into reserved", public: true, shared: load{waiting: sharedQueueLimit}, want: "shared", reject: true},
		{name: "unknown cannot claim tier zero", unknown: []string{"warm", "sibling"}, shared: preferred, want: "shared"},
		{name: "known busy beats unknown shared", warm: busy, sibling: busy, unknown: []string{"shared"}, want: "warm"},
		{name: "known busy shared beats unknown reserved", unknown: []string{"warm", "sibling"}, shared: busy, want: "shared"},
		{name: "all unknown fail open", unknown: []string{"warm", "sibling", "shared"}, want: "warm"},
		{name: "unknown public cannot use reserved", public: true, unknown: []string{"shared"}, want: "shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTieredTestModel()
			for host, load := range map[string]load{"warm": tc.warm, "sibling": tc.sibling, "shared": tc.shared} {
				observeCounts(m.Enclaves[host].metrics, load.waiting, load.running)
			}
			for _, host := range tc.unknown {
				metrics := m.Enclaves[host].metrics
				counts, _ := metrics.latestSample()
				metrics.updateLatest(counts, time.Now().Add(-sampleStalenessLimit-time.Second))
			}
			org := reservationOverloadOrg
			if tc.public {
				org = "public-org"
			}
			primary, spill, policy := m.ReservationPools(Caller{OrgID: org})
			order := []string{"other-reserved", "warm", "sibling", "shared"}
			selected, claim, reject, retry, _ := m.SelectServing(order, true, primary, spill, policy)
			if selected != nil {
				selected.ReleaseDispatch()
			}
			if selected == nil || selected.host != tc.want || claim != nil || reject != tc.reject {
				t.Fatalf("selected=%v claim=%v reject=%v, want %s reject=%v", selected, claim, reject, tc.want, tc.reject)
			}
			if reject && retry != time.Minute {
				t.Fatalf("retry=%v, want default one minute", retry)
			}
		})
	}
}

func TestWaitingOnlyPolicyUsesTierOrder(t *testing.T) {
	const queueLimit = 3
	m := newTieredTestModel()
	policy := &config.OverloadConfig{MaxRequestsWaiting: queueLimit, RetryAfterMinutes: sharedRetryMinutes}
	m.applyReservations("gpt-oss-120b", []config.ReservationConfig{{
		OrgIDs: []string{reservationOverloadOrg}, Enclaves: []string{"warm", "sibling"}, Overload: policy,
	}}, []string{"warm", "sibling", "shared"})
	primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
	for _, host := range []string{"warm", "sibling"} {
		m.Enclaves[host].metrics.configurePolicies(policy, nil)
		observe(m.Enclaves[host].metrics, queueLimit-1)
	}
	m.Enclaves["shared"].metrics.configurePolicies(m.Overload, []*config.OverloadConfig{policy})
	observe(m.Enclaves["shared"].metrics, 0)
	order := []string{"warm", "sibling", "shared"}
	if selected, _, reject, _, _ := m.SelectServing(order, true, primary, spill, policy); selected.host != "shared" || reject {
		t.Fatal("queued reserved hosts must yield to shared tier zero without a running threshold")
	} else {
		selected.ReleaseDispatch()
	}
	for _, host := range []string{"warm", "sibling", "shared"} {
		observe(m.Enclaves[host].metrics, queueLimit)
	}
	if selected, _, reject, retry, _ := m.SelectServing(order, true, primary, spill, policy); selected.host != "warm" || !reject || retry != sharedRetryMinutes*time.Minute {
		t.Fatal("all pools must use the caller's queue and retry policy")
	} else {
		selected.ReleaseDispatch()
	}
}

func TestShortReservedQueueStaysReserved(t *testing.T) {
	const queueLimit = 10
	type load struct{ waiting, running float64 }
	idle, queued := load{}, load{waiting: 1, running: 1}
	deep := load{waiting: reservedQueueTolerance + 1, running: 1}
	full := load{waiting: 1, running: runningRequestLimit - 1}
	for _, tc := range []struct {
		name          string
		warm, sibling load
		inflight      int64
		want, retry   string
	}{
		{name: "short queue stays reserved", warm: queued, sibling: queued, want: "warm", retry: "sibling"},
		{name: "deep queue spills", warm: deep, sibling: deep, want: "shared", retry: "shared"},
		{name: "reserved tier zero beats short queue", warm: queued, sibling: idle, want: "sibling", retry: "sibling"},
		{name: "queue reaching running limit spills", warm: full, sibling: full, want: "shared", retry: "shared"},
		{name: "local in-flight at running limit spills", warm: queued, sibling: queued, inflight: runningRequestLimit, want: "shared", retry: "shared"},
		{name: "queue guard spills", warm: load{waiting: queueLimit}, sibling: load{waiting: queueLimit}, want: "shared", retry: "shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTieredTestModel()
			policy := &config.OverloadConfig{SoftMaxRequestsRunning: runningRequestLimit, MaxRequestsWaiting: queueLimit}
			m.applyReservations("gpt-oss-120b", []config.ReservationConfig{{
				OrgIDs: []string{reservationOverloadOrg}, Enclaves: []string{"warm", "sibling"}, Overload: policy,
			}}, []string{"warm", "sibling", "shared"})
			for host, l := range map[string]load{"warm": tc.warm, "sibling": tc.sibling, "shared": idle} {
				metrics := m.Enclaves[host].metrics
				if host == "shared" {
					metrics.configurePolicies(m.Overload, []*config.OverloadConfig{policy})
				} else {
					metrics.configurePolicies(policy, nil)
					m.Enclaves[host].inflight.Store(tc.inflight)
				}
				observeCounts(metrics, l.waiting, l.running)
			}
			primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
			order := []string{"warm", "sibling", "shared"}
			selected, _, reject, _, _ := m.SelectServing(order, true, primary, spill, policy)
			if selected != nil {
				selected.ReleaseDispatch()
			}
			if selected == nil || selected.host != tc.want || reject {
				t.Fatalf("selected=%v reject=%v, want %s", selected, reject, tc.want)
			}
			retry := m.SelectRetry("warm", order, primary, spill, policy)
			if retry != nil {
				retry.ReleaseDispatch()
			}
			if retry == nil || retry.host != tc.retry {
				t.Fatalf("retry=%v, want %s", retry, tc.retry)
			}
		})
	}
}

func TestTieredSelectionHealthAndAffinityCap(t *testing.T) {
	m := newTieredTestModel()
	primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
	order := []string{"warm", "sibling"}
	cap := 0
	m.CacheRoute = &config.CacheRouteConfig{MaxInflightDelta: &cap}
	m.Enclaves["warm"].inflight.Store(1)
	if selected, _, _, _, _ := m.SelectServing(order, true, primary, spill, policy); selected.host != "sibling" {
		t.Fatal("affinity bypassed the local in-flight cap")
	} else {
		selected.ReleaseDispatch()
	}
	for _, host := range []string{"warm", "sibling"} {
		e := m.Enclaves[host]
		e.attestation = newAttestationState(time.Now().Add(-time.Second), time.Now(), "expired-release")
		e.cb.storeState(cbOpen)
		e.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	}
	if selected, claim, _, _, _ := m.SelectServing(order, true, primary, spill, policy); selected.host != "shared" || claim != nil {
		t.Fatal("expired evidence was selected or claimed for a probe")
	} else {
		selected.ReleaseDispatch()
	}
	m.Enclaves["shared"].attestation = newAttestationState(time.Now().Add(-time.Second), time.Now(), "expired-release")
	if selected, _, _, _, _ := m.SelectServing(order, true, primary, spill, policy); selected != nil {
		t.Fatal("unavailable caller pools escaped to another organization's reservation")
	}
}

func TestConcurrentSelectionsReserveLocalLoad(t *testing.T) {
	const requests = 64
	m := newTieredTestModel()
	cap := 0
	m.CacheRoute = &config.CacheRouteConfig{MaxInflightDelta: &cap}
	primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
	// This test exercises affinity-balanced local load, not soft-max
	// spillover (see TestLocalInflightSpillsBurstInsidePollInterval), so
	// keep every tier preferred for the whole burst.
	policy.SoftMaxRequestsRunning = requests
	start := make(chan struct{})
	results := make(chan *Enclave, requests)
	for range requests {
		go func() {
			<-start
			enclave, _, _, _, _ := m.SelectServing([]string{"warm", "sibling"}, true, primary, spill, policy)
			results <- enclave
		}()
	}
	close(start)
	selected := make([]*Enclave, 0, requests)
	for range requests {
		selected = append(selected, <-results)
	}
	for host, enclave := range m.Enclaves {
		want := int64(0)
		if primary[host] {
			want = requests / int64(len(primary))
		}
		if got := enclave.inflight.Load(); got != want {
			t.Errorf("%s in-flight before dispatch = %d, want %d", host, got, want)
		}
	}
	for _, enclave := range selected {
		if enclave == nil {
			t.Error("available capacity returned no enclave")
			continue
		}
		enclave.ReleaseDispatch()
	}
	for host, enclave := range m.Enclaves {
		if got := enclave.inflight.Load(); got != 0 {
			t.Errorf("%s in-flight after completion = %d, want zero", host, got)
		}
	}
}

func TestTieredRecoveryProbes(t *testing.T) {
	for _, claimProbes := range []bool{false, true} {
		m := newTieredTestModel()
		primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
		probe := m.Enclaves["warm"]
		probe.cb.storeState(cbOpen)
		probe.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
		observeCounts(probe.metrics, reservedQueueLimit, runningRequestLimit)
		selected, claim, _, _, _ := m.SelectServing([]string{"warm", "sibling"}, claimProbes, primary, spill, policy)
		if selected != nil {
			selected.ReleaseDispatch()
		}
		if claimProbes {
			if selected != probe || claim == nil {
				t.Fatal("recovery probe did not bypass tier rejection")
			}
		} else if selected.host != "sibling" || claim != nil {
			t.Fatal("dispatch without breaker outcomes claimed a probe")
		}
	}
	m := newTieredTestModel()
	primary, spill, policy := m.ReservationPools(Caller{OrgID: reservationOverloadOrg})
	shared := m.Enclaves["shared"]
	shared.cb.storeState(cbOpen)
	shared.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
	if selected, claim, _, _, _ := m.SelectServing([]string{"warm"}, true, primary, spill, policy); selected.host != "warm" || claim != nil {
		t.Fatal("shared recovery probe displaced available reserved tier zero")
	} else {
		selected.ReleaseDispatch()
	}
}

func TestTierPoliciesKeepIndependentQueueRecovery(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(http.NotFound))
	defer server.Close()
	shared := &config.OverloadConfig{SoftMaxRequestsRunning: 2 * runningRequestLimit, MaxRequestsWaiting: sharedQueueLimit}
	reserved := &config.OverloadConfig{SoftMaxRequestsRunning: runningRequestLimit, MaxRequestsWaiting: reservedQueueLimit, RetryAfterMinutes: sharedRetryMinutes}
	m := newTestMetrics(strings.TrimPrefix(server.URL, "https://"), shared)
	m.client = server.Client()
	defer m.shutdown()
	m.setPolicies(shared, []*config.OverloadConfig{reserved})
	for _, step := range []struct {
		waiting          float64
		shared, reserved routingTier
	}{
		{sharedQueueLimit, tierOverloaded, tierOverloaded},
		{sharedQueueLimit - 1, tierOverloaded, tierOverloaded},
		{sharedQueueLimit / 2, tierBusy, tierOverloaded},
		{0, tierPreferred, tierPreferred},
		{sharedQueueLimit - 1, tierBusy, tierOverloaded},
	} {
		observeCounts(m, step.waiting, 0)
		m.setPolicies(shared, []*config.OverloadConfig{reserved})
		for _, check := range []struct {
			policy *config.OverloadConfig
			want   routingTier
		}{{shared, step.shared}, {reserved, step.reserved}} {
			if tier, _ := m.tier(check.policy, 0); tier != check.want {
				t.Fatalf("waiting=%v policy=%+v tier=%v, want %v", step.waiting, check.policy, tier, check.want)
			}
		}
	}
	m.setPolicies(shared, nil)
	if len(m.queueOverloads) != 1 {
		t.Fatal("removed policies retained hysteresis state")
	}
}
