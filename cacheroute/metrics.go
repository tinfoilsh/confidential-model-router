package cacheroute

import (
	"encoding/hex"
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-model-router/cachesalt"
	"github.com/tinfoilsh/confidential-model-router/config"
)

// Every label below is a closed enum. A routing key, or anything derived
// from one, must never appear as a label value: it would be unbounded
// cardinality and a stable pseudonymous user identifier.
var (
	// RequestsTotal is the eligibility funnel. outcome="error" counts
	// recovered panics and must stay ≈ 0.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_requests_total",
			Help: "Requests seen by the cache-route pipeline, by eligibility outcome (keyed, no_salt, no_prefix, below_floor, pool_too_small, error)",
		},
		[]string{"model", "outcome"},
	)

	// ReuseTotal classifies keyed requests against where the actual pick
	// landed. repeat_cold = the prefix was warm on a replica the pick
	// missed, i.e. the reuse cache-aware routing would have captured.
	ReuseTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_reuse_total",
			Help: "Keyed requests by prefix-reuse classification against the actual pick (first_seen, repeat_warm, repeat_cold)",
		},
		[]string{"model", "outcome"},
	)

	// ReusePromptBytesTotal weights ReuseTotal by prompt bytes, since
	// prefill cost scales with prompt size.
	ReusePromptBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_reuse_prompt_bytes_total",
			Help: "Prompt bytes of keyed requests by prefix-reuse classification (first_seen, repeat_warm, repeat_cold)",
		},
		[]string{"model", "outcome"},
	)

	// PicksTotal counts the would-be pick per enclave, with the key's
	// replication factor at pick time. Requests served from outside the
	// ranked membership (breaker probes) are excluded, so this is also the
	// denominator for the match rate.
	PicksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_picks_total",
			Help: "Would-be cache-aware picks by enclave and replication factor at pick time",
		},
		[]string{"model", "enclave", "r"},
	)

	// RandomMatchTotal counts keyed requests whose would-be pick equals
	// the actual random pick; expect RandomMatchTotal/PicksTotal ≈
	// 1/pool-size.
	RandomMatchTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_random_match_total",
			Help: "Keyed requests whose would-be cache-aware pick equals the actual random pick",
		},
		[]string{"model"},
	)

	// RepeatIntervalSeconds measures time between sightings of a key.
	// Buckets straddle every plausible retention window.
	RepeatIntervalSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "router_cache_route_repeat_interval_seconds",
			Help:    "Time since a routing key was last seen, observed on repeats",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600},
		},
		[]string{"model"},
	)

	// KeyRPM is the per-key request rate observed at request time.
	// Buckets straddle every plausible split threshold.
	KeyRPM = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "router_cache_route_key_rpm",
			Help:    "Observed per-key request rate (requests/minute) at request time",
			Buckets: []float64{1, 2, 5, 10, 15, 30, 60, 120, 300},
		},
		[]string{"model"},
	)

	// KeyEvictionsTotal separates normal turnover (ttl) from table
	// pressure (capacity). Capacity evictions make genuine repeats look
	// first-seen and bias reuse counts low — keep ≈ 0 by sizing the table.
	KeyEvictionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_key_evictions_total",
			Help: "Shadow-table key evictions by reason (ttl, capacity)",
		},
		[]string{"model", "reason"},
	)

	// ComputeSeconds is the shadow path's per-request cost; p99 should
	// stay well under 1ms.
	ComputeSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "router_cache_route_compute_seconds",
			Help:    "Wall time spent in the cache-route shadow path per request",
			Buckets: []float64{1e-6, 5e-6, 1e-5, 5e-5, 1e-4, 5e-4, 1e-3, 5e-3, 1e-2},
		},
	)

	// PoolInfo pins each model's configured replica-set view. Router
	// instances that disagree on pool_hash compute different HRW rankings
	// for the same key.
	PoolInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "router_cache_route_pool_info",
			Help: "Constant 1, labeled with a short hash of the model's sorted configured replica hostnames",
		},
		[]string{"model", "pool_hash"},
	)

	// ModeInfo reports the effective cache-route mode per model, after any
	// clamping.
	ModeInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "router_cache_route_mode",
			Help: "Constant 1, labeled with the effective cache-route mode for the model (off, shadow, enforced)",
		},
		[]string{"model", "mode"},
	)

	// trackedKeysDesc describes the gauge Shadow.Collect emits at scrape
	// time: live table keys by HRW home and replication factor.
	trackedKeysDesc = prometheus.NewDesc(
		"router_cache_route_tracked_keys",
		"Live shadow-table routing keys by HRW home enclave and current replication factor",
		[]string{"model", "enclave", "r"},
		nil,
	)
)

// poolInfoDomainTag namespaces the pool_hash derivation; no tag in the
// router may be a prefix of another.
const poolInfoDomainTag = "tinfoil/route-pool/v1"

// infoMu guards the last-published info labels so a change replaces the old
// series instead of leaving a stale one beside it.
var (
	infoMu       sync.Mutex
	lastPoolHash = map[string]string{}
	lastMode     = map[string]Mode{}
)

// SetPoolInfo publishes the pool_info series for a model from its configured
// enclave hostnames (order-insensitive). Empty hostnames delete the series.
func SetPoolInfo(model string, hostnames []string) {
	sorted := make([]string, len(hostnames))
	copy(sorted, hostnames)
	slices.Sort(sorted)
	hash := ""
	if len(sorted) > 0 {
		sum := cachesalt.Sum(poolInfoDomainTag, sorted...)
		hash = hex.EncodeToString(sum[:4])
	}

	infoMu.Lock()
	defer infoMu.Unlock()
	if prev, ok := lastPoolHash[model]; ok && prev != hash {
		PoolInfo.DeleteLabelValues(model, prev)
	}
	if hash == "" {
		delete(lastPoolHash, model)
		return
	}
	lastPoolHash[model] = hash
	PoolInfo.WithLabelValues(model, hash).Set(1)
}

// SetMode publishes the effective mode for a model, logging when it clamps
// a configured "enforced" down to shadow so config intent and router
// behavior can't silently diverge.
func SetMode(model string, cfg *config.CacheRouteConfig) {
	raw := ""
	if cfg != nil {
		raw = cfg.Mode
	}
	mode, clamped := resolveMode(raw)
	if clamped {
		log.WithFields(log.Fields{
			"model": model,
			"mode":  raw,
		}).Warn("cache_route mode 'enforced' is not implemented in this build; running shadow")
	}

	infoMu.Lock()
	defer infoMu.Unlock()
	if prev, ok := lastMode[model]; ok && prev != mode {
		ModeInfo.DeleteLabelValues(model, string(prev))
	}
	lastMode[model] = mode
	ModeInfo.WithLabelValues(model, string(mode)).Set(1)
}

// DropModel removes a model's info series when it leaves the config.
func DropModel(model string) {
	infoMu.Lock()
	defer infoMu.Unlock()
	if prev, ok := lastPoolHash[model]; ok {
		PoolInfo.DeleteLabelValues(model, prev)
		delete(lastPoolHash, model)
	}
	if prev, ok := lastMode[model]; ok {
		ModeInfo.DeleteLabelValues(model, string(prev))
		delete(lastMode, model)
	}
}
