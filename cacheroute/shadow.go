package cacheroute

import (
	"container/list"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DefaultMaxKeys bounds the shadow table. At a few hundred bytes per entry
// this costs tens of MB. Size it so capacity evictions stay ≈ 0 — a table at
// capacity misclassifies genuine repeats as first-seen and understates the
// prize (watch router_cache_route_key_evictions_total{reason="capacity"}).
const DefaultMaxKeys = 100_000

// retentionFactor extends entry lifetime past the classification window W.
// Classification only trusts warmth within W, but the repeat-interval
// histogram exists to validate W against real re-arrival spacing — which
// requires seeing re-arrivals *beyond* W. Expiring entries exactly at W would
// truncate the histogram at the window boundary and make the measurement an
// artifact of itself.
const retentionFactor = 4

// sweepInterval is how often the background sweeper drains expired entries.
// Expiry is also enforced at touch time (stale warmth classifies as
// first-seen), so the sweeper is purely a memory bound, not a correctness
// one.
const sweepInterval = 30 * time.Second

// sweepChunk caps evictions per lock acquisition so a large expired backlog
// (e.g. after an idle night) can't stall the request path behind one long
// sweep.
const sweepChunk = 1024

// Reuse classifications, the outcome label values of ReuseTotal and
// ReusePromptBytesTotal. Closed set; dashboards depend on them.
const (
	// ReuseFirstSeen: the key is not in the table, or nothing is warm for
	// it anymore.
	ReuseFirstSeen = "first_seen"
	// ReuseRepeatWarm: the actual pick was already warm for this key —
	// random routing got the hit anyway.
	ReuseRepeatWarm = "repeat_warm"
	// ReuseRepeatCold: the key is warm somewhere, but not where the pick
	// landed. This is the prize shadow mode exists to size.
	ReuseRepeatCold = "repeat_cold"
)

// Shadow is the cache-aware routing shadow tracker: one bounded, evicting
// table keyed by routing key that answers, at request time, "have we seen
// this key recently, and where did it land?" — the join a per-request
// decision log would have enabled offline, done in-enclave because metrics
// are the only telemetry channel out. Nothing in it is exported except the
// aggregates in metrics.go and the tracked-keys gauge it emits as a
// prometheus.Collector.
type Shadow struct {
	mu      sync.Mutex
	entries map[string]*entry // table key: model \x00 routing key
	lru     *list.List        // front = most recently touched; values are table keys
	maxKeys int

	now  func() time.Time
	done chan struct{}
}

// entry is the per-key shadow state. It never leaves the enclave.
type entry struct {
	model    string
	lastSeen time.Time
	expiry   time.Time

	// warm records, per replica hostname, when this key last landed there
	// — under random routing, "where this prefix is warm". Entries older
	// than W are pruned on touch, so the map stays bounded by the pool
	// size plus briefly-stale hosts.
	warm map[string]time.Time

	// Sliding-window request rate (two fixed one-minute buckets,
	// interpolated), modeled on ratelimit.RequestTracker.
	windowStart time.Time
	prevCount   int
	curCount    int

	// home and r are the key's HRW rank-0 replica and replication factor
	// as of the last observation, aggregated at scrape time into the
	// tracked-keys gauge.
	home string
	r    int

	elem *list.Element
}

// Option configures a Shadow.
type Option func(*Shadow)

// WithNowFunc injects a custom clock (for testing).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Shadow) { s.now = f }
}

// WithMaxKeys overrides the table capacity (for testing).
func WithMaxKeys(n int) Option {
	return func(s *Shadow) { s.maxKeys = n }
}

// NewShadow creates a shadow tracker, registers its tracked-keys gauge with
// reg (nil means the default registerer), and starts the background sweeper.
// Callers own its lifetime: Close stops the sweeper.
func NewShadow(reg prometheus.Registerer, opts ...Option) *Shadow {
	s := &Shadow{
		entries: make(map[string]*entry),
		lru:     list.New(),
		maxKeys: DefaultMaxKeys,
		now:     time.Now,
		done:    make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	reg.MustRegister(s)
	go s.sweeper()
	return s
}

// Close stops the background sweeper.
func (s *Shadow) Close() {
	close(s.done)
}

// Observe runs the shadow pipeline for one request after the production
// selector has made its (still random) pick. It classifies the request
// against the table, computes the would-be cache-aware pick, updates the
// table, and increments metrics. It must never affect the request: any panic
// is recovered into the error outcome, and its full cost is metered into
// ComputeSeconds.
func (s *Shadow) Observe(model string, req *Request, pool Pool, actualHost string, cfg Settings) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			RequestsTotal.WithLabelValues(model, string(OutcomeError)).Inc()
		}
		var extraction time.Duration
		if req != nil {
			extraction = req.elapsed
		}
		ComputeSeconds.Observe((extraction + time.Since(start)).Seconds())
	}()

	if req.Outcome != OutcomeKeyed {
		RequestsTotal.WithLabelValues(model, string(req.Outcome)).Inc()
		return
	}
	if pool.Size < 2 {
		RequestsTotal.WithLabelValues(model, string(OutcomePoolTooSmall)).Inc()
		return
	}
	if len(pool.Candidates) == 0 {
		// Can't happen while the pool snapshot falls back to all
		// replicas, but a ranking over nothing must not panic into the
		// error counter for a shape difference.
		RequestsTotal.WithLabelValues(model, string(OutcomePoolTooSmall)).Inc()
		return
	}

	res := s.observeKeyed(model, req.Key, pool.Candidates, actualHost, cfg)

	RequestsTotal.WithLabelValues(model, string(OutcomeKeyed)).Inc()
	ReuseTotal.WithLabelValues(model, res.reuse).Inc()
	ReusePromptBytesTotal.WithLabelValues(model, res.reuse).Add(float64(req.PromptBytes))
	if res.repeat {
		RepeatIntervalSeconds.WithLabelValues(model).Observe(res.interval.Seconds())
	}
	KeyRPM.WithLabelValues(model).Observe(res.rpm)
	PicksTotal.WithLabelValues(model, res.pick, strconv.Itoa(res.r)).Inc()
	if res.pick == actualHost {
		RandomMatchTotal.WithLabelValues(model).Inc()
	}
}

// keyedResult carries everything observeKeyed computes under the lock so the
// metric increments can happen outside it.
type keyedResult struct {
	reuse    string        // ReuseTotal outcome label
	repeat   bool          // whether the key was in the table (interval is meaningful)
	interval time.Duration // time since the key was last seen
	rpm      float64       // sliding-window request rate, including this request
	pick     string        // would-be pick: least-loaded of the top-R ranking
	r        int           // replication factor at pick time
}

// observeKeyed does the table touch, classification, ranking, and state
// update for one keyed request under a single lock acquisition.
func (s *Shadow) observeKeyed(model string, key Key, candidates []Candidate, actualHost string, cfg Settings) keyedResult {
	now := s.now()
	// The table key is model-scoped: the routing key itself contains no
	// model (§5.1), so the same salted prompt sent to two models derives
	// the same key — but warm hostnames are per-model replicas, and
	// cross-model hits must not pollute each other's classification.
	tableKey := model + "\x00" + string(key[:])

	s.mu.Lock()
	defer s.mu.Unlock()

	var res keyedResult
	e, ok := s.entries[tableKey]
	if !ok {
		e = s.insertLocked(model, tableKey, now)
		res.reuse = ReuseFirstSeen
	} else {
		res.repeat = true
		res.interval = now.Sub(e.lastSeen)
		s.lru.MoveToFront(e.elem)

		// Prune warmth beyond the classification window; what's left is
		// where the engine plausibly still holds this prefix.
		for host, seen := range e.warm {
			if now.Sub(seen) > cfg.Retention {
				delete(e.warm, host)
			}
		}
		switch {
		case len(e.warm) == 0:
			// The key is back, but nothing is warm anymore —
			// equivalent to a first sighting for cache purposes.
			res.reuse = ReuseFirstSeen
		case !e.warm[actualHost].IsZero():
			res.reuse = ReuseRepeatWarm
		default:
			res.reuse = ReuseRepeatCold
		}
	}

	res.rpm = e.bumpRate(now)
	e.warm[actualHost] = now
	e.lastSeen = now
	e.expiry = now.Add(retentionFactor * cfg.Retention)

	ranked := rank(key, candidates)
	res.r = replicationFactor(res.rpm, cfg.SplitThresholdRPM, len(ranked))
	res.pick = leastLoaded(ranked[:res.r]).Host
	e.home = ranked[0].Host
	e.r = res.r

	return res
}

// insertLocked adds a fresh entry, evicting the least-recently-used one when
// the table is full. Must be called with s.mu held.
func (s *Shadow) insertLocked(model, tableKey string, now time.Time) *entry {
	if len(s.entries) >= s.maxKeys {
		if back := s.lru.Back(); back != nil {
			evictKey := back.Value.(string)
			KeyEvictionsTotal.WithLabelValues(s.entries[evictKey].model, "capacity").Inc()
			delete(s.entries, evictKey)
			s.lru.Remove(back)
		}
	}
	e := &entry{
		model: model,
		warm:  make(map[string]time.Time, 4),
	}
	e.elem = s.lru.PushFront(tableKey)
	s.entries[tableKey] = e
	return e
}

// bumpRate records one request in the entry's rate window and returns the
// sliding-window rate in requests/minute, including this request. Two fixed
// one-minute buckets are interpolated by position within the current minute —
// cheap, and smooth enough that the key_rpm histogram and R don't sawtooth at
// minute boundaries.
func (e *entry) bumpRate(now time.Time) float64 {
	minute := now.Truncate(time.Minute)
	switch {
	case e.windowStart.Equal(minute):
		e.curCount++
	case minute.Sub(e.windowStart) == time.Minute:
		e.prevCount = e.curCount
		e.curCount = 1
		e.windowStart = minute
	default: // first request, or a gap of more than a minute
		e.prevCount = 0
		e.curCount = 1
		e.windowStart = minute
	}
	frac := now.Sub(minute).Seconds() / 60
	return float64(e.prevCount)*(1-frac) + float64(e.curCount)
}

// sweeper periodically drops expired entries from the cold end of the LRU.
// Because a touch moves an entry to the front and pushes its expiry forward,
// LRU order is expiry order, so sweeping pops from the back until it meets a
// live entry — O(expired), not O(table).
func (s *Shadow) sweeper() {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

// sweep evicts expired entries in bounded chunks per lock acquisition.
func (s *Shadow) sweep() {
	for {
		evicted := 0
		s.mu.Lock()
		now := s.now()
		for evicted < sweepChunk {
			back := s.lru.Back()
			if back == nil {
				break
			}
			tableKey := back.Value.(string)
			e := s.entries[tableKey]
			if e.expiry.After(now) {
				break
			}
			KeyEvictionsTotal.WithLabelValues(e.model, "ttl").Inc()
			delete(s.entries, tableKey)
			s.lru.Remove(back)
			evicted++
		}
		s.mu.Unlock()
		if evicted < sweepChunk {
			return
		}
	}
}

// Describe implements prometheus.Collector.
func (s *Shadow) Describe(ch chan<- *prometheus.Desc) {
	ch <- trackedKeysDesc
}

// Collect implements prometheus.Collector: it walks the table and emits the
// tracked-keys gauge — live keys by model, current HRW home, and current
// replication factor. Only counts leave the lock; keys never do.
func (s *Shadow) Collect(ch chan<- prometheus.Metric) {
	type group struct{ model, home, r string }
	counts := make(map[group]int)

	s.mu.Lock()
	for _, e := range s.entries {
		counts[group{e.model, e.home, strconv.Itoa(e.r)}]++
	}
	s.mu.Unlock()

	for g, n := range counts {
		ch <- prometheus.MustNewConstMetric(trackedKeysDesc, prometheus.GaugeValue, float64(n), g.model, g.home, g.r)
	}
}
