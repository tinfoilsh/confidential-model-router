package cacheroute

import (
	"container/list"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DefaultMaxKeys bounds the shadow table (a few hundred bytes per entry).
// Size it so capacity evictions stay ≈ 0; see KeyEvictionsTotal.
const DefaultMaxKeys = 100_000

// retentionFactor keeps entries alive past the classification window so
// RepeatIntervalSeconds can see re-arrivals beyond it. Expiring at exactly
// the window would truncate the histogram at the very boundary the data is
// meant to tune.
const retentionFactor = 4

// sweepInterval and sweepChunk bound the background eviction of expired
// entries. Stale warmth is also handled at touch time, so sweeping is a
// memory bound, not a correctness one.
const (
	sweepInterval = 30 * time.Second
	sweepChunk    = 1024
)

// Reuse classifications for ReuseTotal and ReusePromptBytesTotal.
const (
	// ReuseFirstSeen: key not in the table, or nothing warm for it
	// anymore.
	ReuseFirstSeen = "first_seen"
	// ReuseRepeatWarm: the actual pick was already warm for this key.
	ReuseRepeatWarm = "repeat_warm"
	// ReuseRepeatCold: the key was warm somewhere other than the pick —
	// the reuse cache-aware routing would have captured.
	ReuseRepeatCold = "repeat_cold"
)

// Shadow measures what cache-aware routing would do without acting: a
// bounded, evicting table keyed by routing key records where each key's
// requests actually landed, and Observe turns that into the aggregate
// metrics in metrics.go. Per-key state never leaves this process.
type Shadow struct {
	mu      sync.Mutex
	entries map[string]*entry // table key: model \x00 routing key
	lru     *list.List        // front = most recently touched; values are table keys
	maxKeys int

	now  func() time.Time
	done chan struct{}
}

// entry is the per-key state.
type entry struct {
	model    string
	lastSeen time.Time
	expiry   time.Time

	// warm maps replica hostname → when this key last landed there.
	// Entries older than the retention window are pruned on touch.
	warm map[string]time.Time

	// Two-bucket sliding-window request rate.
	windowStart time.Time
	prevCount   int
	curCount    int

	// Last observed HRW home and replication factor, aggregated into the
	// tracked-keys gauge at scrape time.
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
// reg (nil means the default registerer), and starts the background
// sweeper. Close stops the sweeper.
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
// selector has picked a replica: classify the request against the table,
// compute the would-be pick, update state, increment metrics. It must never
// affect the request — panics are recovered into OutcomeError.
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
		// Unreachable while the pool snapshot falls back to all
		// replicas, but never rank over nothing.
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

// keyedResult is what observeKeyed computes under the lock, so the metric
// increments can happen outside it.
type keyedResult struct {
	reuse    string
	repeat   bool // the key was already in the table
	interval time.Duration
	rpm      float64
	pick     string // least-loaded of the top-R ranking
	r        int
}

// observeKeyed does the table touch, classification, and ranking for one
// keyed request under a single lock acquisition.
func (s *Shadow) observeKeyed(model string, key Key, candidates []Candidate, actualHost string, cfg Settings) keyedResult {
	now := s.now()
	// Scope the table key by model: the routing key contains no model id,
	// so the same salted prompt sent to two models collides — but warmth
	// is per-model replicas.
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

		for host, seen := range e.warm {
			if now.Sub(seen) > cfg.Retention {
				delete(e.warm, host)
			}
		}
		switch {
		case len(e.warm) == 0:
			// Everything went stale: cache-wise a first sighting.
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

// insertLocked adds a fresh entry, evicting the least-recently-used one at
// capacity. Caller holds s.mu.
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

// bumpRate records one request and returns the sliding-window rate in
// requests/minute: two fixed one-minute buckets, interpolated by position
// within the current minute so the rate doesn't sawtooth at boundaries.
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

// sweeper drains expired entries until Close.
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

// sweep evicts expired entries from the cold end of the LRU in bounded
// chunks per lock acquisition. Touches move entries to the front and push
// expiry forward, so LRU order is expiry order and the scan is O(expired),
// not O(table).
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

// Collect implements prometheus.Collector: it aggregates live keys by
// (model, home, replication factor). Only counts leave the lock; keys never
// do.
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
