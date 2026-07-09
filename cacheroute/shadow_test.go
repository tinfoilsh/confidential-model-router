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

// counterValue reads a counter child's value.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	return testutil.ToFloat64(c)
}

// counterDelta returns a reader for how much a counter child has grown since
// this call. The promauto counters are process-global and never reset, so
// assertions must be relative or the suite fails under go test -count=2.
func counterDelta(t *testing.T, vec *prometheus.CounterVec, labels ...string) func() float64 {
	t.Helper()
	base := counterValue(t, vec, labels...)
	return func() float64 { return counterValue(t, vec, labels...) - base }
}

func TestObserveClassification(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "cls-" + t.Name()
	cfg := defaultSettings() // W = 10 min
	req := keyedRequest(t, "one", 100)

	reuse := func(outcome string) float64 { return counterValue(t, ReuseTotal, model, outcome) }
	base := [3]float64{reuse(ReuseFirstSeen), reuse(ReuseRepeatWarm), reuse(ReuseRepeatCold)}
	keyed := counterDelta(t, RequestsTotal, model, string(OutcomeKeyed))

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
	if got := keyed(); got != 4 {
		t.Fatalf("keyed = %v, want 4", got)
	}
}

func TestObserveFunnel(t *testing.T) {
	s, _, _ := newTestShadow(t)
	model := "funnel-" + t.Name()
	cfg := defaultSettings()
	noSalt := counterDelta(t, RequestsTotal, model, string(OutcomeNoSalt))
	belowFloor := counterDelta(t, RequestsTotal, model, string(OutcomeBelowFloor))
	tooSmall := counterDelta(t, RequestsTotal, model, string(OutcomePoolTooSmall))
	keyed := counterDelta(t, RequestsTotal, model, string(OutcomeKeyed))

	// Ineligible outcomes pass straight through to the funnel counter.
	s.Observe(model, &Request{Outcome: OutcomeNoSalt}, pool2(), "enclave-a", cfg)
	s.Observe(model, &Request{Outcome: OutcomeBelowFloor}, pool2(), "enclave-a", cfg)
	if got := noSalt(); got != 1 {
		t.Fatalf("no_salt = %v, want 1", got)
	}
	if got := belowFloor(); got != 1 {
		t.Fatalf("below_floor = %v, want 1", got)
	}

	// A keyed request on a single-replica pool: nothing to route.
	req := keyedRequest(t, "solo", 10)
	s.Observe(model, req, Pool{Size: 1, Candidates: hosts("only")}, "only", cfg)
	if got := tooSmall(); got != 1 {
		t.Fatalf("pool_too_small = %v, want 1", got)
	}
	if got := keyed(); got != 0 {
		t.Fatalf("keyed = %v, want 0", got)
	}
}

func TestObserveByteWeighting(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "bytes-" + t.Name()
	cfg := defaultSettings()
	req := keyedRequest(t, "bw", 5000)
	firstBytes := counterDelta(t, ReusePromptBytesTotal, model, ReuseFirstSeen)
	coldBytes := counterDelta(t, ReusePromptBytesTotal, model, ReuseRepeatCold)

	s.Observe(model, req, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), "enclave-b", cfg)

	if got := firstBytes(); got != 5000 {
		t.Fatalf("first_seen bytes = %v, want 5000", got)
	}
	if got := coldBytes(); got != 5000 {
		t.Fatalf("repeat_cold bytes = %v, want 5000", got)
	}
}

// TestObservePanicRecovery proves the shadow path cannot fail a request: a
// nil *Request panics inside Observe and must be swallowed into the error
// outcome.
func TestObservePanicRecovery(t *testing.T) {
	s, _, _ := newTestShadow(t)
	model := "panic-" + t.Name()
	errored := counterDelta(t, RequestsTotal, model, string(OutcomeError))

	s.Observe(model, nil, pool2(), "enclave-a", defaultSettings())

	if got := errored(); got != 1 {
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
	capacityEvictions := counterDelta(t, KeyEvictionsTotal, model, "capacity")

	s.Observe(model, k1, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	s.Observe(model, k2, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	// Touch k1 so k2 is the LRU entry, then insert k3 → k2 evicted.
	s.Observe(model, k1, pool2(), "enclave-a", cfg)
	clock.advance(time.Second)
	s.Observe(model, k3, pool2(), "enclave-a", cfg)

	if got := capacityEvictions(); got != 1 {
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

	ttlEvictions := counterDelta(t, KeyEvictionsTotal, model, "ttl")
	s.Observe(model, keyedRequest(t, "t1", 1), pool2(), "enclave-a", cfg)
	s.Observe(model, keyedRequest(t, "t2", 1), pool2(), "enclave-b", cfg)

	// Entries are retained retentionFactor×W past their last touch so the
	// repeat-interval histogram can see beyond-W re-arrivals; only after
	// that are they swept.
	clock.advance(cfg.Retention + time.Minute)
	s.sweep()
	if got := ttlEvictions(); got != 0 {
		t.Fatalf("swept %v entries before retention elapsed", got)
	}

	clock.advance(time.Duration(retentionFactor) * cfg.Retention)
	s.sweep()
	if got := ttlEvictions(); got != 2 {
		t.Fatalf("ttl evictions = %v, want 2", got)
	}
}

// TestSweepPerRetention pins that an idle long-retention key cannot strand
// expired keys from shorter-retention models behind it.
func TestSweepPerRetention(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	longModel := "sweep-long-" + t.Name()
	shortModel := "sweep-short-" + t.Name()
	long := defaultSettings()
	long.Retention = time.Hour
	short := defaultSettings() // 10 min

	ttlLong := counterDelta(t, KeyEvictionsTotal, longModel, "ttl")
	ttlShort := counterDelta(t, KeyEvictionsTotal, shortModel, "ttl")

	s.Observe(longModel, keyedRequest(t, "L", 1), pool2(), "enclave-a", long)
	clock.advance(time.Minute)
	s.Observe(shortModel, keyedRequest(t, "S1", 1), pool2(), "enclave-a", short)
	s.Observe(shortModel, keyedRequest(t, "S2", 1), pool2(), "enclave-b", short)

	// Past the short model's full retention but well inside the long one's.
	clock.advance(time.Duration(retentionFactor)*short.Retention + time.Minute)
	s.sweep()
	if got := ttlShort(); got != 2 {
		t.Fatalf("short-retention ttl evictions = %v, want 2", got)
	}
	if got := ttlLong(); got != 0 {
		t.Fatalf("long-retention ttl evictions = %v, want 0", got)
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

	matches := counterDelta(t, RandomMatchTotal, model)
	s.Observe(model, req, pool2(), "enclave-a", cfg)
	pick := "enclave-a"
	other := "enclave-b"
	if matches() == 0 {
		pick, other = other, pick
	}

	afterInference := matches()
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), pick, cfg)
	if got := matches() - afterInference; got != 1 {
		t.Fatalf("match on pick = %v, want 1", got)
	}
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), other, cfg)
	if got := matches() - afterInference; got != 1 {
		t.Fatalf("non-pick must not count as match, delta = %v", got)
	}
}

// TestObserveProbeServed pins that a request served from outside the ranked
// membership (a breaker probe) updates reuse and warmth but is excluded from
// the pick-vs-random comparison.
func TestObserveProbeServed(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "probe-served-" + t.Name()
	cfg := defaultSettings()
	req := keyedRequest(t, "p", 1)

	keyed := counterDelta(t, RequestsTotal, model, string(OutcomeKeyed))
	matches := counterDelta(t, RandomMatchTotal, model)
	picks := func() float64 {
		total := 0.0
		for _, host := range []string{"enclave-a", "enclave-b", "probe-c"} {
			for _, r := range []string{"1", "2"} {
				if c, err := PicksTotal.GetMetricWithLabelValues(model, host, r); err == nil {
					total += testutil.ToFloat64(c)
				}
			}
		}
		return total
	}
	picksBase := picks()

	// Served by a probing replica that is not a candidate.
	s.Observe(model, req, pool2(), "probe-c", cfg)
	if got := keyed(); got != 1 {
		t.Fatalf("keyed = %v, want 1", got)
	}
	if got := picks() - picksBase; got != 0 {
		t.Fatalf("picks after probe dispatch = %v, want 0", got)
	}
	if got := matches(); got != 0 {
		t.Fatalf("matches after probe dispatch = %v, want 0", got)
	}

	// The probe still warmed its replica: a repeat onto it is warm.
	warm := counterDelta(t, ReuseTotal, model, ReuseRepeatWarm)
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), "probe-c", cfg)
	if got := warm(); got != 1 {
		t.Fatalf("repeat onto probed replica: warm = %v, want 1", got)
	}

	// A normally-served request resumes the comparison.
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), "enclave-a", cfg)
	if got := picks() - picksBase; got != 1 {
		t.Fatalf("picks after normal dispatch = %v, want 1", got)
	}
}

// TestDecide pins the enforcement ordering contract: the pick first, then
// the rest of the ranking, covering every candidate exactly once;
// ineligible requests yield nil so callers fall back to random.
func TestDecide(t *testing.T) {
	s, _, _ := newTestShadow(t)
	model := "order-" + t.Name()
	cfg := defaultSettings()
	req := keyedRequest(t, "o", 1)
	pool := Pool{Size: 3, Candidates: hosts("enclave-a", "enclave-b", "enclave-c")}

	d := s.Decide(model, req, pool, cfg)
	if d == nil || len(d.Order) != 3 {
		t.Fatalf("decision = %+v, want an order over all 3 candidates", d)
	}
	seen := map[string]bool{}
	for _, h := range d.Order {
		if seen[h] {
			t.Fatalf("order = %v, duplicate host", d.Order)
		}
		seen[h] = true
	}
	for range 5 {
		again := s.Decide(model, req, pool, cfg)
		for i := range d.Order {
			if again.Order[i] != d.Order[i] {
				t.Fatalf("order not deterministic: %v vs %v", again.Order, d.Order)
			}
		}
	}

	// For a cold key R=1, so the pick is the ranking's head.
	if want := rankedHosts(req.Key, pool.Candidates)[0]; d.Order[0] != want {
		t.Fatalf("order[0] = %s, want ranking head %s", d.Order[0], want)
	}
	if d.home != d.Order[0] || d.r != 1 {
		t.Fatalf("decision home/r = %s/%d, want %s/1", d.home, d.r, d.Order[0])
	}

	if got := s.Decide(model, &Request{Outcome: OutcomeNoSalt}, pool, cfg); got != nil {
		t.Fatalf("non-keyed request must yield nil decision, got %+v", got)
	}
	if got := s.Decide(model, req, Pool{Size: 1, Candidates: hosts("only")}, cfg); got != nil {
		t.Fatalf("single-replica pool must yield nil decision, got %+v", got)
	}
	if got := s.Decide(model, nil, pool, cfg); got != nil {
		t.Fatalf("nil request must yield nil decision, got %+v", got)
	}
}

// TestDecideHotKey drives a key past the split threshold and pins that the
// order starts with the least-loaded of the top-R set.
func TestDecideHotKey(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "order-hot-" + t.Name()
	cfg := defaultSettings() // threshold 15 rpm
	req := keyedRequest(t, "oh", 1)

	// Establish a hot rate through observations.
	for range 40 {
		clock.advance(time.Second)
		s.Observe(model, req, pool2(), "enclave-a", cfg)
	}

	// Find the cold home: order with idle candidates starts there.
	home := s.Decide(model, req, pool2(), cfg).Order[0]
	other := "enclave-b"
	if home == other {
		other = "enclave-a"
	}

	// With the home busier than its sibling, R=2 must steer the order to
	// the least-loaded of the pair.
	loaded := Pool{Size: 2, Candidates: []Candidate{
		{Host: home, InFlight: 5},
		{Host: other, InFlight: 0},
	}}
	d := s.Decide(model, req, loaded, cfg)
	if d.Order[0] != other {
		t.Fatalf("hot key order = %v, want least-loaded %s first", d.Order, other)
	}
	if d.r != 2 {
		t.Fatalf("hot key r = %d, want 2", d.r)
	}
}

// TestObserveLanding pins the decision-threading contract: picks_total is
// labeled with the landing and the replication factor that shaped routing,
// the random comparison stays quiet, and the same-host repeat is warm.
func TestObserveLanding(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "landing-" + t.Name()
	cfg := defaultSettings()
	cfg.Mode = ModeEnforced
	req := keyedRequest(t, "l", 1)

	matches := counterDelta(t, RandomMatchTotal, model)
	landedPicks := counterDelta(t, PicksTotal, model, "enclave-b", "1")
	warm := counterDelta(t, ReuseTotal, model, ReuseRepeatWarm)

	d := s.Decide(model, req, pool2(), cfg)
	s.ObserveLanding(model, req, d, "enclave-b", cfg)
	clock.advance(time.Second)
	d = s.Decide(model, req, pool2(), cfg)
	s.ObserveLanding(model, req, d, "enclave-b", cfg)

	if got := landedPicks(); got != 2 {
		t.Fatalf("picks for landing = %v, want 2", got)
	}
	if got := matches(); got != 0 {
		t.Fatalf("random match under enforcement = %v, want 0", got)
	}
	if got := warm(); got != 1 {
		t.Fatalf("repeat_warm = %v, want 1", got)
	}
}

// TestObserveLandingUsesDecisionR pins that picks_total is labeled with the
// replication factor that routed the request, not one recomputed after the
// arrival bumps the rate: near the split threshold the two differ.
func TestObserveLandingUsesDecisionR(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "decision-r-" + t.Name()
	cfg := defaultSettings()
	cfg.Mode = ModeEnforced
	cfg.SplitThresholdRPM = 3
	req := keyedRequest(t, "dr", 1)

	// Three arrivals in one minute, then step into the next minute: the
	// pre-arrival rate reads just under the threshold (r=1) while the
	// post-arrival rate crosses it (r=2).
	for range 3 {
		clock.advance(time.Second)
		s.Observe(model, req, pool2(), "enclave-a", cfg)
	}
	clock.advance(time.Minute)

	r1Picks := counterDelta(t, PicksTotal, model, "enclave-a", "1")
	d := s.Decide(model, req, pool2(), cfg)
	if d.r != 1 {
		t.Fatalf("decision r = %d, want 1 (pre-arrival rate under threshold)", d.r)
	}
	s.ObserveLanding(model, req, d, "enclave-a", cfg)
	if got := r1Picks(); got != 1 {
		t.Fatalf("picks labeled with routed r=1: %v, want 1 (post-arrival recompute would label r=2)", got)
	}
}

// TestObserveEnforced pins enforcement metric semantics: picks_total records
// the landing, and the random comparison stays quiet.
func TestObserveEnforced(t *testing.T) {
	s, clock, _ := newTestShadow(t)
	model := "enforced-" + t.Name()
	cfg := defaultSettings()
	cfg.Mode = ModeEnforced
	req := keyedRequest(t, "e", 1)

	matches := counterDelta(t, RandomMatchTotal, model)
	landedPicks := counterDelta(t, PicksTotal, model, "enclave-b", "1")
	warm := counterDelta(t, ReuseTotal, model, ReuseRepeatWarm)

	// Land twice on enclave-b regardless of what the ranking would pick.
	s.Observe(model, req, pool2(), "enclave-b", cfg)
	clock.advance(time.Second)
	s.Observe(model, req, pool2(), "enclave-b", cfg)

	if got := landedPicks(); got != 2 {
		t.Fatalf("picks for landing = %v, want 2", got)
	}
	if got := matches(); got != 0 {
		t.Fatalf("random match under enforcement = %v, want 0", got)
	}

	// The second landing on the same host is the enforcement success
	// signal: repeat_warm, not repeat_cold.
	if got := warm(); got != 1 {
		t.Fatalf("repeat_warm = %v, want 1", got)
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
	SetMode(model, &config.CacheRouteConfig{Mode: "enforced"})
	if got := gatherLabelValues(t, ModeInfo, model, "mode"); len(got) != 1 || !got["enforced"] {
		t.Fatalf("mode = %v, want enforced", got)
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
