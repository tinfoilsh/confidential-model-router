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

const runningRequestLimit = 4

func observeCounts(m *enclaveMetrics, waiting, running float64) {
	counts := requestCounts{waiting: waiting, running: running, hasWaiting: true, hasRunning: true}
	m.updateLatest(counts, time.Now())
}

func TestExtractRequestCounts(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       requestCounts
		wantErr    bool
	}{
		{
			name: "labeled gauges with timestamps and multiple engines",
			body: `# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="gpt-oss-120b",engine="0"} 1 123456789
vllm:num_requests_running{model_name="gpt-oss-120b",engine="1"} 3 123456789
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="gpt-oss-120b"} 0
`,
			want: requestCounts{running: runningRequestLimit, hasWaiting: true, hasRunning: true},
		},
		{name: "running without waiting", body: runningMetricName + " 4\n", want: requestCounts{running: runningRequestLimit, hasRunning: true}},
		{name: "waiting without running", body: waitingMetricName + " 1\n", want: requestCounts{waiting: 1, hasWaiting: true}},
		{name: "similarly named metric ignored", body: runningMetricName + "_total 4\n"},
		{name: "missing gauges", body: "# HELP other Other metric.\n"},
		{name: "malformed", body: runningMetricName + " bad\n", wantErr: true},
		{name: "nan", body: runningMetricName + " NaN\n", wantErr: true},
		{name: "infinity", body: runningMetricName + " +Inf\n", wantErr: true},
		{name: "negative", body: runningMetricName + " -1\n", wantErr: true},
		{name: "counter", body: "# TYPE " + runningMetricName + " counter\n" + runningMetricName + " 4\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractRequestCounts(strings.NewReader(tc.body))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("counts=%+v err=%v, want %+v err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestScrapeRunningPreference(t *testing.T) {
	var running atomic.Int64
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %d\n%s 0\n", runningMetricName, running.Load(), waitingMetricName)
	}))
	defer ts.Close()
	m := newTestMetrics(strings.TrimPrefix(ts.URL, "https://"), &config.OverloadConfig{
		MaxRequestsRunning: runningRequestLimit, RetryAfterMinutes: 2,
	})
	m.client = ts.Client()
	for _, count := range []int{runningRequestLimit - 1, runningRequestLimit, runningRequestLimit + 1, runningRequestLimit - 1} {
		running.Store(int64(count))
		m.scrape(t.Context())
		if got := testutil.ToFloat64(BackendRunningRequests.WithLabelValues(m.model, m.host)); got != float64(count) {
			t.Fatalf("running gauge=%v, want %d", got, count)
		}
		want := tierPreferred
		if count >= runningRequestLimit {
			want = tierBusy
		}
		if tier, _ := m.tier(m.nativePolicy); tier != want {
			t.Fatalf("running=%d tier=%v, want %v", count, tier, want)
		}
	}
}

func TestCombinedOverloadHysteresis(t *testing.T) {
	m := newTestMetrics("combined-limits.example", &config.OverloadConfig{
		MaxRequestsWaiting: 4, ClearRequestsWaiting: 1, MaxRequestsRunning: runningRequestLimit,
	})
	for _, step := range []struct {
		waiting, running float64
		reject           bool
	}{
		{0, 3, false},
		{0, 4, false},
		{0, 3, false},
		{0, 4, false},
		{2, 2, false},
		{1, 2, false},
		{4, 0, true},
		{2, 0, true},
		{1, 4, false},
		{1, 3, false},
		{1, 2, false},
	} {
		observeCounts(m, step.waiting, step.running)
		if tier, _ := m.tier(m.nativePolicy); (tier == tierOverloaded) != step.reject {
			t.Fatalf("waiting=%v running=%v reject=%v, want %v", step.waiting, step.running, tier == tierOverloaded, step.reject)
		}
	}
}

func TestRunningScrapeFailuresPreserveLastSample(t *testing.T) {
	previous := requestCounts{waiting: 1, running: runningRequestLimit, hasWaiting: true, hasRunning: true}
	for _, tc := range []struct {
		name, body    string
		status        int
		wantTelemetry requestCounts
	}{
		{name: "missing running", body: waitingMetricName + " 0\n", status: http.StatusOK, wantTelemetry: requestCounts{hasWaiting: true}},
		{name: "missing waiting", body: runningMetricName + " 0\n", status: http.StatusOK, wantTelemetry: requestCounts{hasRunning: true}},
		{name: "invalid running", body: runningMetricName + " NaN\n", status: http.StatusOK, wantTelemetry: previous},
		{name: "HTTP error", body: runningMetricName + " 0\n" + waitingMetricName + " 0\n", status: http.StatusServiceUnavailable, wantTelemetry: previous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer ts.Close()
			m := newTestMetrics(strings.TrimPrefix(ts.URL, "https://"), &config.OverloadConfig{
				MaxRequestsWaiting: 1, MaxRequestsRunning: runningRequestLimit,
			})
			m.client = ts.Client()
			observeCounts(m, previous.waiting, previous.running)
			waitingGauge := BackendQueueDepth.WithLabelValues(m.model, m.host)
			runningGauge := BackendRunningRequests.WithLabelValues(m.model, m.host)
			waitingGauge.Set(previous.waiting)
			runningGauge.Set(previous.running)
			counts, collected := m.latestSample()
			m.scrape(t.Context())
			telemetry := requestCounts{
				hasWaiting: BackendQueueDepth.DeleteLabelValues(m.model, m.host),
				hasRunning: BackendRunningRequests.DeleteLabelValues(m.model, m.host),
			}
			if telemetry.hasWaiting {
				telemetry.waiting = testutil.ToFloat64(waitingGauge)
			}
			if telemetry.hasRunning {
				telemetry.running = testutil.ToFloat64(runningGauge)
			}
			if telemetry != tc.wantTelemetry {
				t.Fatalf("telemetry=%+v, want %+v", telemetry, tc.wantTelemetry)
			}
			if got, at := m.latestSample(); got != counts || at != collected {
				t.Fatalf("failed scrape replaced the last sample: %+v at %v", got, at)
			}
			if tier, _ := m.tier(m.nativePolicy); tier != tierOverloaded {
				t.Fatal("failed scrape cleared a fresh overload observation")
			}
			m.updateLatest(counts, time.Now().Add(-sampleStalenessLimit-time.Second))
			if tier, _ := m.tier(m.nativePolicy); tier != tierUnknown {
				t.Fatal("stale running sample did not fail open")
			}
		})
	}
}

func TestRunningPolicyReload(t *testing.T) {
	m := newTestMetrics("reload.example", &config.OverloadConfig{MaxRequestsWaiting: reservedQueueLimit})
	observeCounts(m, 0, runningRequestLimit)
	for _, limit := range []int{runningRequestLimit, runningRequestLimit + 1, runningRequestLimit} {
		policy := &config.OverloadConfig{MaxRequestsRunning: limit}
		m.configurePolicies(policy, nil)
		want := tierBusy
		if limit > runningRequestLimit {
			want = tierPreferred
		}
		if tier, _ := m.tier(policy); tier != want {
			t.Fatalf("running limit=%d tier=%v, want %v", limit, tier, want)
		}
	}
}
