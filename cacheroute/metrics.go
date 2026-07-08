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

// The Phase 4 metric contract (implementation plan, Phase 4). Names are
// phase-neutral — the same pipeline keeps emitting them when Phase 5 flips to
// acting, so the flip is verified on the same Grafana panels. Every label is
// a closed enum; per-key values must never appear as labels (a key-derived
// label would be a stable pseudonymous user identifier and unbounded
// cardinality). Per-enclave counters are the same exposure class as the
// existing router_proxy_success_total{model, enclave}.
var (
	// RequestsTotal is the eligibility funnel — the denominator for every
	// other panel, and (via outcome="error") proof the shadow path never
	// fails a request.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_requests_total",
			Help: "Requests seen by the cache-route pipeline, by eligibility outcome (keyed, no_salt, no_prefix, below_floor, pool_too_small, error)",
		},
		[]string{"model", "outcome"},
	)

	// ReuseTotal classifies each keyed request against where the actual
	// pick landed: first_seen, repeat_warm (the pick was already warm for
	// this key), repeat_cold (the key was warm somewhere else — the prize
	// shadow mode exists to size; under enforcement it collapsing toward
	// zero is the success signal).
	ReuseTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_reuse_total",
			Help: "Keyed requests by prefix-reuse classification against the actual pick (first_seen, repeat_warm, repeat_cold)",
		},
		[]string{"model", "outcome"},
	)

	// ReusePromptBytesTotal is ReuseTotal weighted by prompt bytes, the
	// prefill-cost proxy: read the prize byte-weighted first.
	ReusePromptBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_reuse_prompt_bytes_total",
			Help: "Prompt bytes of keyed requests by prefix-reuse classification (first_seen, repeat_warm, repeat_cold)",
		},
		[]string{"model", "outcome"},
	)

	// PicksTotal is the would-be pick per keyed request with the key's
	// replication factor at pick time. Balance across enclaves answers
	// exit question 2; the r split answers question 3.
	PicksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_picks_total",
			Help: "Would-be cache-aware picks by enclave and replication factor at pick time",
		},
		[]string{"model", "enclave", "r"},
	)

	// RandomMatchTotal counts keyed requests where the would-be pick
	// equals the production random pick: ≈ 1/N is the pipeline sanity
	// check, and 1 − match rate sizes how much traffic moves at the
	// Phase 5 flip. Goes quiet under enforcement.
	RandomMatchTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_random_match_total",
			Help: "Keyed requests whose would-be cache-aware pick equals the actual random pick",
		},
		[]string{"model"},
	)

	// RepeatIntervalSeconds validates W against real re-arrival spacing.
	// Buckets must straddle every candidate W (1 s – 30 min); the shadow
	// table retains keys past W so mass beyond the window is visible
	// rather than truncated at it.
	RepeatIntervalSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "router_cache_route_repeat_interval_seconds",
			Help:    "Time since a routing key was last seen, observed on repeats",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600},
		},
		[]string{"model"},
	)

	// KeyRPM places the hot-key split threshold before anything acts.
	// Buckets must straddle the candidate threshold (1 – 300 rpm;
	// OpenAI's figure is ~15).
	KeyRPM = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "router_cache_route_key_rpm",
			Help:    "Observed per-key request rate (requests/minute) at request time",
			Buckets: []float64{1, 2, 5, 10, 15, 30, 60, 120, 300},
		},
		[]string{"model"},
	)

	// KeyEvictionsTotal separates normal turnover (ttl) from classification
	// bias (capacity): a table at capacity misclassifies genuine repeats
	// as first-seen and understates the prize, so capacity must sit ≈ 0.
	KeyEvictionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_route_key_evictions_total",
			Help: "Shadow-table key evictions by reason (ttl, capacity)",
		},
		[]string{"model", "reason"},
	)

	// ComputeSeconds is the full shadow-path cost per request (extraction
	// + observation) — the "shadow is free" proof, needed because there is
	// no profiler in the enclave either. p99 must stay well under 1 ms.
	ComputeSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "router_cache_route_compute_seconds",
			Help:    "Wall time spent in the cache-route shadow path per request",
			Buckets: []float64{1e-6, 5e-6, 1e-5, 5e-5, 1e-4, 5e-4, 1e-3, 5e-3, 1e-2},
		},
	)

	// PoolInfo pins each model's configured replica-set view. Equal
	// pool_hash across router instances ⇒ identical HRW views, and with
	// keyless derivation (§6) identical views ⇒ identical homes by
	// construction — the continuous replacement for a cross-router
	// spot-check.
	PoolInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "router_cache_route_pool_info",
			Help: "Constant 1, labeled with a short hash of the model's sorted configured replica hostnames",
		},
		[]string{"model", "pool_hash"},
	)

	// ModeInfo exposes the per-pool rollout state (off, shadow, enforced)
	// so dashboards read the phase from data, not deploy folklore. It
	// reports the effective mode this build runs, after any clamping.
	ModeInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "router_cache_route_mode",
			Help: "Constant 1, labeled with the effective cache-route mode for the model (off, shadow, enforced)",
		},
		[]string{"model", "mode"},
	)

	// trackedKeysDesc describes the router_cache_route_tracked_keys gauge,
	// emitted by Shadow's Collect: live shadow-table keys by current HRW
	// home and replication factor — the distinct-key view for exit
	// questions 2 and 3. It is a custom collector because the values are
	// walked from the table at scrape time.
	trackedKeysDesc = prometheus.NewDesc(
		"router_cache_route_tracked_keys",
		"Live shadow-table routing keys by HRW home enclave and current replication factor",
		[]string{"model", "enclave", "r"},
		nil,
	)
)

// poolInfoDomainTag namespaces the pool_hash derivation. Same constraint as
// every other tag: fixed constant, no tag a prefix of another.
const poolInfoDomainTag = "tinfoil/route-pool/v1"

// infoMu guards lastPoolHash and lastMode, which remember each model's
// current info-label values so a change replaces the old series instead of
// leaving a stale one alongside it.
var (
	infoMu       sync.Mutex
	lastPoolHash = map[string]string{}
	lastMode     = map[string]Mode{}
)

// SetPoolInfo publishes the pool_info series for a model from its configured
// enclave hostnames (order-insensitive), replacing any previous series. Empty
// hostnames delete the series — the model has no pool to agree on.
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

// SetMode publishes the mode series for a model from its raw config block,
// replacing any previous series. It logs when it clamps a configured
// "enforced" down to shadow (this build measures only; Phase 5 acts), so the
// config's intent and the router's behavior can't silently diverge.
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
