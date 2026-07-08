package manager

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/tinfoilsh/confidential-model-router/config"
)

// newTestMetrics returns an enclaveMetrics with the given overload config
// installed directly, without starting the polling goroutine.
func newTestMetrics(host string, cfg *config.OverloadConfig) *enclaveMetrics {
	m := newEnclaveMetrics(host, "test-model")
	m.cfgMu.Lock()
	m.cfg = cfg
	m.cfgMu.Unlock()
	return m
}

// observe simulates one scrape: record the sample as fresh and evaluate.
func observe(m *enclaveMetrics, waiting float64) {
	m.updateLatest(waiting, time.Now())
	m.evaluateThresholds(waiting)
}

func TestEvaluateThresholds_Hysteresis(t *testing.T) {
	m := newTestMetrics("hysteresis-host", &config.OverloadConfig{MaxRequestsWaiting: 16})

	steps := []struct {
		waiting    float64
		overloaded bool
		desc       string
	}{
		{15, false, "below trip mark, never tripped"},
		{9, false, "inside band from below holds clear"},
		{16, true, "trip mark reached"},
		{15, true, "inside band holds overloaded"},
		{9, true, "just above clear mark still holds"},
		{8, false, "clear mark (trip/2) reached"},
		{15, false, "inside band from below holds clear again"},
		{20, true, "re-trips above trip mark"},
		{0, false, "empty queue clears"},
	}
	for _, step := range steps {
		observe(m, step.waiting)
		if got := m.overloaded.Load(); got != step.overloaded {
			t.Fatalf("waiting=%v (%s): overloaded=%v, want %v", step.waiting, step.desc, got, step.overloaded)
		}
	}
}

func TestEvaluateThresholds_ExplicitClearMark(t *testing.T) {
	m := newTestMetrics("explicit-clear-host", &config.OverloadConfig{
		MaxRequestsWaiting:   16,
		ClearRequestsWaiting: 12,
	})

	observe(m, 16)
	if !m.overloaded.Load() {
		t.Fatal("expected trip at 16")
	}
	observe(m, 13)
	if !m.overloaded.Load() {
		t.Fatal("expected 13 to hold overloaded with clear mark 12")
	}
	observe(m, 12)
	if m.overloaded.Load() {
		t.Fatal("expected clear at 12")
	}
}

func TestEvaluateThresholds_TransitionCountersOncePerEpisode(t *testing.T) {
	m := newTestMetrics("transition-count-host", &config.OverloadConfig{MaxRequestsWaiting: 16})
	// The shared counters carry values across test reruns in one process,
	// so assert growth, not absolutes.
	trips := OverloadEventsTotal.WithLabelValues("test-model", "transition-count-host")
	recoveries := RecoveryEventsTotal.WithLabelValues("test-model", "transition-count-host")
	tripBase := testutil.ToFloat64(trips)
	recoveryBase := testutil.ToFloat64(recoveries)

	// One episode: trip once, oscillate inside the band, then drain.
	for _, waiting := range []float64{16, 12, 15, 9, 14, 16, 12} {
		observe(m, waiting)
	}
	if got := testutil.ToFloat64(trips) - tripBase; got != 1 {
		t.Fatalf("trip events during episode = %v, want 1", got)
	}
	if got := testutil.ToFloat64(recoveries) - recoveryBase; got != 0 {
		t.Fatalf("recovery events during episode = %v, want 0", got)
	}

	observe(m, 8)
	observe(m, 3)
	if got := testutil.ToFloat64(recoveries) - recoveryBase; got != 1 {
		t.Fatalf("recovery events after drain = %v, want 1", got)
	}
	if got := testutil.ToFloat64(trips) - tripBase; got != 1 {
		t.Fatalf("trip events after drain = %v, want 1", got)
	}
}

func TestEvaluateThresholds_NoConfigLeavesFlagAlone(t *testing.T) {
	m := newTestMetrics("no-config-host", nil)
	observe(m, 100)
	if m.overloaded.Load() {
		t.Fatal("expected no overload evaluation without config")
	}

	m = newTestMetrics("zero-threshold-host", &config.OverloadConfig{MaxRequestsWaiting: 0})
	observe(m, 100)
	if m.overloaded.Load() {
		t.Fatal("expected no overload evaluation with non-positive trip mark")
	}
}

func TestShouldReject_UsesHystereticFlag(t *testing.T) {
	m := newTestMetrics("reject-host", &config.OverloadConfig{
		MaxRequestsWaiting: 16,
		RetryAfterMinutes:  2,
	})

	observe(m, 12)
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow below trip mark before any episode")
	}

	observe(m, 16)
	reject, retryAfter, waiting := m.shouldReject()
	if !reject {
		t.Fatal("expected reject at trip mark")
	}
	if retryAfter != 2*time.Minute {
		t.Fatalf("retryAfter = %v, want 2m", retryAfter)
	}
	if waiting != 16 {
		t.Fatalf("waiting = %v, want 16", waiting)
	}

	// Inside the band the flag holds, so the reject decision holds too:
	// a tripped backend keeps shedding load until the queue drains.
	observe(m, 12)
	if reject, _, _ := m.shouldReject(); !reject {
		t.Fatal("expected reject to hold inside hysteresis band")
	}

	observe(m, 8)
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow once drained to clear mark")
	}
}

func TestShouldReject_DefaultRetryAfter(t *testing.T) {
	m := newTestMetrics("default-retry-host", &config.OverloadConfig{MaxRequestsWaiting: 16})
	observe(m, 16)
	reject, retryAfter, _ := m.shouldReject()
	if !reject {
		t.Fatal("expected reject at trip mark")
	}
	if retryAfter != time.Minute {
		t.Fatalf("retryAfter = %v, want 1m default", retryAfter)
	}
}

func TestSetConfig_DisabledThresholdResetsFlag(t *testing.T) {
	m := newTestMetrics("disable-reset-host", &config.OverloadConfig{MaxRequestsWaiting: 16})
	observe(m, 16)
	if !m.overloaded.Load() {
		t.Fatal("expected trip at 16")
	}

	// Zeroing the trip mark while keeping the overload block must reset the
	// flag rather than freeze it true with evaluation stopped — a frozen
	// flag would de-prefer the enclave in NextEnclave forever.
	m.setConfig(&config.OverloadConfig{MaxRequestsWaiting: 0, RetryAfterMinutes: 1})
	if m.overloaded.Load() {
		t.Fatal("expected overload flag reset when trip mark is unset")
	}
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow once thresholds are disabled")
	}
	if got := testutil.ToFloat64(BackendOverloaded.WithLabelValues("test-model", "disable-reset-host")); got != 0 {
		t.Fatalf("overloaded gauge = %v, want 0", got)
	}

	// Removing the overload block entirely resets as well.
	m.overloaded.Store(true)
	m.setConfig(nil)
	if m.overloaded.Load() {
		t.Fatal("expected overload flag reset when overload config removed")
	}
}

// TestOverloadTransitionLogMessagesStable pins the transition log message
// strings: they predate hysteresis and external log-based alerts may key on
// them, so renaming them would silently orphan those alerts.
func TestOverloadTransitionLogMessagesStable(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()

	m := newTestMetrics("log-stable-host", &config.OverloadConfig{MaxRequestsWaiting: 16})
	observe(m, 16)
	observe(m, 8)

	var messages []string
	for _, entry := range hook.AllEntries() {
		messages = append(messages, entry.Message)
	}
	if !slices.Contains(messages, "overload threshold exceeded") {
		t.Fatalf("trip log message missing or renamed; got %q", messages)
	}
	if !slices.Contains(messages, "queue depth back below overload threshold") {
		t.Fatalf("recovery log message missing or renamed; got %q", messages)
	}
}

// TestSetConfig_ReevaluatesFlagAgainstNewMarks pins that a threshold change
// takes effect at setConfig time, not at the next successful scrape: the
// server 404s every scrape, so only the synchronous re-evaluation inside
// setConfig can move the flag.
func TestSetConfig_ReevaluatesFlagAgainstNewMarks(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	m := newEnclaveMetrics(strings.TrimPrefix(ts.URL, "https://"), "test-model")
	m.client = ts.Client()
	m.cfgMu.Lock()
	m.cfg = &config.OverloadConfig{MaxRequestsWaiting: 8}
	m.cfgMu.Unlock()

	observe(m, 10) // trips under trip=8, and freshens the cached sample
	if !m.overloaded.Load() {
		t.Fatal("expected trip under the old thresholds")
	}

	// Raising the marks must clear the flag immediately (10 <= derived
	// clear 50), so requests stop being rejected on the old threshold.
	m.setConfig(&config.OverloadConfig{MaxRequestsWaiting: 100, RetryAfterMinutes: 1})
	defer m.shutdown()
	if m.overloaded.Load() {
		t.Fatal("expected flag cleared against raised thresholds at setConfig time")
	}
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow immediately after the threshold raise")
	}

	// Lowering the marks below the cached sample must trip immediately.
	m.setConfig(&config.OverloadConfig{MaxRequestsWaiting: 8, RetryAfterMinutes: 1})
	if !m.overloaded.Load() {
		t.Fatal("expected trip against lowered thresholds at setConfig time")
	}
	if reject, _, _ := m.shouldReject(); !reject {
		t.Fatal("expected reject immediately after the threshold cut")
	}
}

func TestSetConfig_WarnsOnOutOfRangeClearMark(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "vllm:num_requests_waiting 0.0")
	}))
	defer ts.Close()

	hook := logtest.NewGlobal()
	defer hook.Reset()

	m := newEnclaveMetrics(strings.TrimPrefix(ts.URL, "https://"), "test-model")
	m.client = ts.Client()
	m.setConfig(&config.OverloadConfig{
		MaxRequestsWaiting:   16,
		ClearRequestsWaiting: 20, // >= trip: invalid, resolves to the default 8
		RetryAfterMinutes:    1,
	})
	defer m.shutdown()

	for _, entry := range hook.AllEntries() {
		if entry.Message == "clear_requests_waiting out of range, using default" {
			if got := entry.Data["resolved_clear"]; got != 8 {
				t.Fatalf("resolved_clear = %v, want 8", got)
			}
			return
		}
	}
	t.Fatal("expected out-of-range clear mark warning")
}

// TestScrape_EndToEndHysteresis drives the real scrape pipeline — HTTP fetch,
// vLLM exposition parsing, threshold evaluation — against a fake metrics
// endpoint and observes the reject decision across a full overload episode.
func TestScrape_EndToEndHysteresis(t *testing.T) {
	var depth atomic.Int64
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "# HELP vllm:num_requests_waiting Number of requests waiting.\n")
		fmt.Fprintf(w, "vllm:num_requests_waiting{model_name=\"test\"} %d.0\n", depth.Load())
	}))
	defer ts.Close()

	m := newTestMetrics(strings.TrimPrefix(ts.URL, "https://"), &config.OverloadConfig{
		MaxRequestsWaiting: 16,
		RetryAfterMinutes:  1,
	})
	m.client = ts.Client()

	poll := func(d int64) {
		depth.Store(d)
		m.scrape(t.Context())
	}

	poll(12)
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow below trip mark")
	}

	poll(16)
	reject, retryAfter, waiting := m.shouldReject()
	if !reject || retryAfter != time.Minute || waiting != 16 {
		t.Fatalf("at trip mark: reject=%v retryAfter=%v waiting=%v, want true 1m 16", reject, retryAfter, waiting)
	}

	poll(12)
	if reject, _, _ := m.shouldReject(); !reject {
		t.Fatal("expected reject to hold inside hysteresis band")
	}

	poll(8)
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow once drained to clear mark")
	}
}

func TestShouldReject_FailsOpen(t *testing.T) {
	// No config.
	m := newTestMetrics("open-nil-host", nil)
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow without config")
	}

	// Config but no sample yet.
	m = newTestMetrics("open-nosample-host", &config.OverloadConfig{MaxRequestsWaiting: 16})
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow without a sample")
	}

	// Tripped flag but stale sample: fail open.
	m = newTestMetrics("open-stale-host", &config.OverloadConfig{MaxRequestsWaiting: 16})
	observe(m, 16)
	m.updateLatest(16, time.Now().Add(-sampleStalenessLimit-time.Second))
	if reject, _, _ := m.shouldReject(); reject {
		t.Fatal("expected allow with a stale sample even while flagged overloaded")
	}
	if !m.overloaded.Load() {
		t.Fatal("staleness must not clear the overload flag itself")
	}
}
