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

// newTestMetrics installs the real policy state without starting a poller.
func newTestMetrics(host string, policy *config.OverloadConfig) *enclaveMetrics {
	m := newEnclaveMetrics(host, "test-model")
	m.configurePolicies(policy, nil)
	return m
}

func observe(m *enclaveMetrics, waiting float64) {
	m.updateLatest(requestCounts{waiting: waiting, hasWaiting: true}, time.Now())
}

func TestQueueRecoveryAndTelemetry(t *testing.T) {
	const queueLimit = 16
	for _, clear := range []int{0, 12} {
		policy := &config.OverloadConfig{MaxRequestsWaiting: queueLimit, ClearRequestsWaiting: clear}
		m := newTestMetrics(fmt.Sprintf("recovery-%d.example", clear), policy)
		trips := OverloadEventsTotal.WithLabelValues(m.model, m.host)
		recoveries := RecoveryEventsTotal.WithLabelValues(m.model, m.host)
		tripBase, recoveryBase := testutil.ToFloat64(trips), testutil.ToFloat64(recoveries)
		_, resolvedClear := policy.Marks()
		for _, step := range []struct {
			waiting int
			want    routingTier
		}{
			{queueLimit - 1, tierBusy},
			{queueLimit, tierOverloaded},
			{queueLimit - 1, tierOverloaded},
			{resolvedClear + 1, tierOverloaded},
			{resolvedClear, tierBusy},
			{0, tierPreferred},
		} {
			observe(m, float64(step.waiting))
			if tier, _ := m.tier(policy); tier != step.want {
				t.Fatalf("clear=%d waiting=%d tier=%v, want %v", clear, step.waiting, tier, step.want)
			}
			wantGauge := float64(0)
			if step.want == tierOverloaded {
				wantGauge = 1
			}
			if got := testutil.ToFloat64(BackendOverloaded.WithLabelValues(m.model, m.host)); got != wantGauge {
				t.Fatalf("telemetry=%v, want %v from the same recovery state", got, wantGauge)
			}
		}
		if testutil.ToFloat64(trips)-tripBase != 1 || testutil.ToFloat64(recoveries)-recoveryBase != 1 {
			t.Fatal("queue oscillation counted more than one overload/recovery episode")
		}
	}
}

func TestPolicyReloadReevaluatesSample(t *testing.T) {
	const initialLimit = 8
	server := httptest.NewTLSServer(http.HandlerFunc(http.NotFound))
	defer server.Close()
	m := newTestMetrics(strings.TrimPrefix(server.URL, "https://"), &config.OverloadConfig{MaxRequestsWaiting: initialLimit})
	m.client = server.Client()
	defer m.shutdown()
	observe(m, initialLimit+1)
	BackendQueueDepth.WithLabelValues(m.model, m.host).Set(initialLimit + 1)
	BackendRunningRequests.WithLabelValues(m.model, m.host).Set(runningRequestLimit)
	for _, step := range []struct {
		policy *config.OverloadConfig
		want   routingTier
	}{
		{&config.OverloadConfig{MaxRequestsWaiting: 2 * initialLimit}, tierBusy},
		{&config.OverloadConfig{MaxRequestsWaiting: initialLimit}, tierOverloaded},
		{&config.OverloadConfig{}, tierPreferred},
		{&config.OverloadConfig{MaxRequestsWaiting: initialLimit}, tierUnknown},
		{nil, tierPreferred},
	} {
		m.setPolicies(step.policy, nil)
		if tier, _ := m.tier(step.policy); tier != step.want || pollingActive(m) != step.policy.Enabled() {
			t.Fatalf("policy=%+v tier=%v polling=%v, want tier=%v", step.policy, tier, pollingActive(m), step.want)
		}
		if !step.policy.Enabled() && (len(m.queueOverloads) != 0 || testutil.ToFloat64(BackendOverloaded.WithLabelValues(m.model, m.host)) != 0) {
			t.Fatal("disabling the policy retained overload state")
		}
		if !step.policy.Enabled() {
			if BackendQueueDepth.DeleteLabelValues(m.model, m.host) {
				t.Fatal("disabling polling retained a queue depth series")
			}
			if BackendRunningRequests.DeleteLabelValues(m.model, m.host) {
				t.Fatal("disabling polling retained a running requests series")
			}
		}
	}
}

func TestScrapeQueueRecovery(t *testing.T) {
	const queueLimit = 16
	var depth atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "%s{model_name=\"gpt-oss-120b\"} %d\n", waitingMetricName, depth.Load())
	}))
	defer server.Close()
	policy := &config.OverloadConfig{MaxRequestsWaiting: queueLimit}
	m := newTestMetrics(strings.TrimPrefix(server.URL, "https://"), policy)
	m.client = server.Client()
	for _, step := range []struct {
		waiting int64
		want    routingTier
	}{
		{queueLimit - 1, tierBusy}, {queueLimit, tierOverloaded},
		{queueLimit - 1, tierOverloaded}, {queueLimit / 2, tierBusy}, {0, tierPreferred},
	} {
		depth.Store(step.waiting)
		m.scrape(t.Context())
		if tier, waiting := m.tier(policy); tier != step.want || waiting != float64(step.waiting) {
			t.Fatalf("tier=%v waiting=%v, want %v %d", tier, waiting, step.want, step.waiting)
		}
	}
}
