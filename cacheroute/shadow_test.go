package cacheroute

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/tinfoilsh/confidential-model-router/config"
)

// testClock is an injectable clock for the shadow table.
type testClock struct{ t time.Time }

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestShadow builds a Shadow on a private registry with a fake clock.
func newTestShadow(t *testing.T, opts ...Option) (*Shadow, *testClock, *prometheus.Registry) {
	t.Helper()
	clock := newTestClock()
	reg := prometheus.NewRegistry()
	s := NewShadow(reg, append([]Option{WithNowFunc(clock.now)}, opts...)...)
	t.Cleanup(s.Close)
	return s, clock, reg
}

// keyedRequest fabricates a keyed request with a distinct key.
func keyedRequest(t *testing.T, id string, promptBytes int) *Request {
	t.Helper()
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "prefix-" + id + strings.Repeat("x", 4096)},
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	req := ExtractRequest(body, "/v1/chat/completions", testSalt, defaultSettings())
	if req.Outcome != OutcomeKeyed {
		t.Fatalf("fixture not keyed: %s", req.Outcome)
	}
	req.PromptBytes = promptBytes
	return req
}

func pool2() Pool {
	return Pool{Size: 2, Candidates: hosts("enclave-a", "enclave-b")}
}

// counterDelta reads a counter child's value.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	return testutil.ToFloat64(c)
}

func TestObserveClassification(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "cls-" + t.Name()
	cfg := defaultSettings() // W = 10 min
	req := keyedRequest(t, "one", 100)

	reuse := func(outcome string) float64 { return counterValue(t, ReuseTotal, model, outcome) }
	base := [3]float64{reuse(ReuseFirstSeen), reuse(ReuseRepeatWarm), reuse(ReuseRepeatCold)}

	// Never seen: first_seen; enclave-a becomes warm.
	s.Observe(model, req, pool2(), "enclave-a", cfg)
	if got := reuse(ReuseFirstSeen) - base[0]; got != 1 {
		t.Fatalf("first_seen = %v, want 1", got)
	}

	// Repeat onto the warm replica: random routing already got the hit.
	clock.advance(time.Minute)
	s.Observe(model, req, pool2(), "enclave-a", cfg)
	if got := reuse(ReuseRepeatWarm) - base[1]; got != 1 {
		t.Fatalf("repeat_warm = %v, want 1", got)
	}

	// Repeat onto the cold replica while a is warm: the prize.
	clock.advance(time.Minute)
	s.Observe(model, req, pool2(), "enclave-b", cfg)
	if got := reuse(ReuseRepeatCold) - base[2]; got != 1 {
		t.Fatalf("repeat_cold = %v, want 1", got)
	}

	// Past W everything is stale: cache-wise a first sighting again.
	clock.advance(cfg.Retention + time.Minute)
	s.Observe(model, req, pool2(), "enclave-a", cfg)
	if got := reuse(ReuseFirstSeen) - base[0]; got != 2 {
		t.Fatalf("first_seen after W = %v, want 2", got)
	}

	// All four keyed requests counted in the funnel.
	if got := counterValue(t, RequestsTotal, model, string(OutcomeKeyed)); got != 4 {
		t.Fatalf("keyed = %v, want 4", got)
	}
}

func TestObserveFunnel(t *testing.T) {
	s, _, _ := newTestShadow(t)
	model := "funnel-" + t.Name()
	cfg := defaultSettings()

	// Ineligible outcomes pass straight through to the funnel counter.
	s.Observe(model, &Request{Outcome: OutcomeNoSalt}, pool2(), "enclave-a", cfg)
	s.Observe(model, &Request{Outcome: OutcomeBelowFloor}, pool2(), "enclave-a", cfg)
	if got := counterValue(t, RequestsTotal, model, string(OutcomeNoSalt)); got != 1 {
		t.Fatalf("no_salt = %v, want 1", got)
	}
	if got := counterValue(t, RequestsTotal, model, string(OutcomeBelowFloor)); got != 1 {
		t.Fatalf("below_floor = %v, want 1", got)
	}

	// A keyed request on a single-replica pool: nothing to route.
	req := keyedRequest(t, "solo", 10)
	s.Observe(model, req, Pool{Size: 1, Candidates: hosts("only")}, "only", cfg)
	if got := counterValue(t, RequestsTotal, model, string(OutcomePoolTooSmall)); got != 1 {
		t.Fatalf("pool_too_small = %v, want 1", got)
	}
	if got := counterValue(t, RequestsTotal, model, string(OutcomeKeyed)); got != 0 {
		t.Fatalf("keyed = %v, want 0", got)
	}
}

func TestObserveByteWeighting(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "bytes-" + t.Name()
	cfg := defaultSettings()
	req := keyedRequest(t, "bw", 5000)

	s.Observe(model, req, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), "enclave-b", cfg)

	if got := counterValue(t, ReusePromptBytesTotal, model, ReuseFirstSeen); got != 5000 {
		t.Fatalf("first_seen bytes = %v, want 5000", got)
	}
	if got := counterValue(t, ReusePromptBytesTotal, model, ReuseRepeatCold); got != 5000 {
		t.Fatalf("repeat_cold bytes = %v, want 5000", got)
	}
}

// TestObservePanicRecovery proves the shadow path cannot fail a request: a
// nil *Request panics inside Observe and must be swallowed into the error
// outcome.
func TestObservePanicRecovery(t *testing.T) {
	s, _, _ := newTestShadow(t)
	model := "panic-" + t.Name()

	s.Observe(model, nil, pool2(), "enclave-a", defaultSettings())

	if got := counterValue(t, RequestsTotal, model, string(OutcomeError)); got != 1 {
		t.Fatalf("error = %v, want 1", got)
	}
}

func TestCapacityEviction(t *testing.T) {
	s, clock, _ := newTestShadow(t, WithMaxKeys(2))
	model := "cap-" + t.Name()
	cfg := defaultSettings()

	k1 := keyedRequest(t, "k1", 1)
	k2 := keyedRequest(t, "k2", 1)
	k3 := keyedRequest(t, "k3", 1)

	s.Observe(model, k1, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	s.Observe(model, k2, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	// Touch k1 so k2 is the LRU entry, then insert k3 → k2 evicted.
	s.Observe(model, k1, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	s.Observe(model, k3, pool2(), "enclave-a", cfg)

	if got := counterValue(t, KeyEvictionsTotal, model, "capacity"); got != 1 {
		t.Fatalf("capacity evictions = %v, want 1", got)
	}

	// k1 survived (repeat), k2 was evicted (first_seen again) — the
	// documented capacity bias.
	warmBefore := counterValue(t, ReuseTotal, model, ReuseRepeatWarm)
	clock.advance(time.Second)
	s.Observe(model, k1, pool2(), "enclave-a", cfg)
	if got := counterValue(t, ReuseTotal, model, ReuseRepeatWarm) - warmBefore; got != 1 {
		t.Fatalf("k1 must still be tracked, warm delta = %v", got)
	}
	firstBefore := counterValue(t, ReuseTotal, model, ReuseFirstSeen)
	clock.advance(time.Second)
	s.Observe(model, k2, pool2(), "enclave-a", cfg)
	if got := counterValue(t, ReuseTotal, model, ReuseFirstSeen) - firstBefore; got != 1 {
		t.Fatalf("evicted k2 must classify first_seen, delta = %v", got)
	}
}

func TestTTLSweep(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "ttl-" + t.Name()
	cfg := defaultSettings()

	s.Observe(model, keyedRequest(t, "t1", 1), pool2(), "enclave-a", cfg)
	s.Observe(model, keyedRequest(t, "t2", 1), pool2(), "enclave-b", cfg)

	// Entries are retained retentionFactor×W past their last touch so the
	// repeat-interval histogram can see beyond-W re-arrivals; only after
	// that are they swept.
	clock.advance(cfg.Retention + time.Minute)
	s.sweep()
	if got := counterValue(t, KeyEvictionsTotal, model, "ttl"); got != 0 {
		t.Fatalf("swept %v entries before retention elapsed", got)
	}

	clock.advance(time.Duration(retentionFactor) * cfg.Retention)
	s.sweep()
	if got := counterValue(t, KeyEvictionsTotal, model, "ttl"); got != 2 {
		t.Fatalf("ttl evictions = %v, want 2", got)
	}
}

// TestHotKeyReplication drives one key past the split threshold and expects
// its replication factor to grow — and a slow key's to stay at 1.
func TestHotKeyReplication(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "hot-" + t.Name()
	cfg := defaultSettings() // threshold 15 rpm
	hot := keyedRequest(t, "hot", 1)

	// ~40 requests inside one minute: rpm crosses 15, R clamps to pool
	// size 2.
	for range 40 {
		clock.advance(time.Second)
		s.Observe(model, hot, pool2(), "enclave-a", cfg)
	}

	splitPicks := 0.0
	for _, host := range []string{"enclave-a", "enclave-b"} {
		if c, err := PicksTotal.GetMetricWithLabelValues(model, host, "2"); err == nil {
			splitPicks += testutil.ToFloat64(c)
		}
	}
	if splitPicks == 0 {
		t.Fatal("hot key never reached replication factor 2")
	}

	cold := keyedRequest(t, "cold", 1)
	s.Observe(model, cold, pool2(), "enclave-a", cfg)
	r1 := 0.0
	for _, host := range []string{"enclave-a", "enclave-b"} {
		if c, err := PicksTotal.GetMetricWithLabelValues(model, host, "1"); err == nil {
			r1 += testutil.ToFloat64(c)
		}
	}
	if r1 == 0 {
		t.Fatal("slow keys must keep replication factor 1")
	}
}

// TestRandomMatch verifies the pick-vs-random agreement counter by inferring
// the deterministic pick from the first observation.
func TestRandomMatch(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "match-" + t.Name()
	cfg := defaultSettings()
	req := keyedRequest(t, "m", 1)

	s.Observe(model, req, pool2(), "enclave-a", cfg)
	pick := "enclave-a"
	other := "enclave-b"
	if counterValue(t, RandomMatchTotal, model) == 0 {
		pick, other = other, pick
	}

	base := counterValue(t, RandomMatchTotal, model)
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), pick, cfg)
	if got := counterValue(t, RandomMatchTotal, model) - base; got != 1 {
		t.Fatalf("match on pick = %v, want 1", got)
	}
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), other, cfg)
	if got := counterValue(t, RandomMatchTotal, model) - base; got != 1 {
		t.Fatalf("non-pick must not count as match, delta = %v", got)
	}
}

// TestTrackedKeysCollector checks the scrape-time gauge: distinct live keys
// grouped by home enclave and replication factor.
func TestTrackedKeysCollector(t *testing.T) {
	s, clock, reg := newTestShadow(t)
	model := "tracked-" + t.Name()
	cfg := defaultSettings()

	for i := range 10 {
		clock.advance(time.Second)
		s.Observe(model, keyedRequest(t, string(rune('a'+i)), 1), pool2(), "enclave-a", cfg)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	homes := map[string]bool{}
	for _, mf := range families {
		if mf.GetName() != "router_cache_route_tracked_keys" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["model"] != model {
				continue
			}
			if labels["r"] != "1" {
				t.Errorf("unexpected replication factor %s for cold keys", labels["r"])
			}
			homes[labels["enclave"]] = true
			total += m.GetGauge().GetValue()
		}
	}
	if total != 10 {
		t.Fatalf("tracked keys = %v, want 10", total)
	}
	if len(homes) == 0 {
		t.Fatal("no home enclaves reported")
	}
}

// TestPoolAndModeInfo exercises the config-sync info series: value changes
// replace the old series rather than accumulating, and DropModel removes
// them.
func TestPoolAndModeInfo(t *testing.T) {
	model := "info-" + t.Name()

	SetPoolInfo(model, []string{"h2", "h1"})
	SetPoolInfo(model, []string{"h1", "h2"}) // order-insensitive: same hash
	if got := gatherLabelValues(t, PoolInfo, model, "pool_hash"); len(got) != 1 {
		t.Fatalf("pool_hash series = %v, want exactly one", got)
	}
	SetPoolInfo(model, []string{"h1", "h2", "h3"})
	if got := gatherLabelValues(t, PoolInfo, model, "pool_hash"); len(got) != 1 {
		t.Fatalf("pool change must replace the series, got %v", got)
	}

	SetMode(model, nil)
	if got := gatherLabelValues(t, ModeInfo, model, "mode"); len(got) != 1 || !got["off"] {
		t.Fatalf("mode = %v, want off", got)
	}
	SetMode(model, &config.CacheRouteConfig{Mode: "shadow"})
	if got := gatherLabelValues(t, ModeInfo, model, "mode"); len(got) != 1 || !got["shadow"] {
		t.Fatalf("mode = %v, want shadow", got)
	}
	// The gauge reports the effective mode: enforced clamps to shadow.
	SetMode(model, &config.CacheRouteConfig{Mode: "enforced"})
	if got := gatherLabelValues(t, ModeInfo, model, "mode"); len(got) != 1 || !got["shadow"] {
		t.Fatalf("mode = %v, want shadow (clamped)", got)
	}

	DropModel(model)
	if got := gatherLabelValues(t, PoolInfo, model, "pool_hash"); len(got) != 0 {
		t.Fatalf("dropped model still has pool series: %v", got)
	}
	if got := gatherLabelValues(t, ModeInfo, model, "mode"); len(got) != 0 {
		t.Fatalf("dropped model still has mode series: %v", got)
	}
}

// gatherLabelValues collects the values of one label across a vector's
// series for a model, via the default registerer.
func gatherLabelValues(t *testing.T, vec *prometheus.GaugeVec, model, label string) map[string]bool {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() { vec.Collect(ch); close(ch) }()
	out := map[string]bool{}
	for m := range ch {
		var d dto.Metric
		if err := m.Write(&d); err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{}
		for _, lp := range d.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["model"] == model {
			out[labels[label]] = true
		}
	}
	return out
}
