package manager

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/tinfoilsh/confidential-model-router/config"
)

type shadowTier int

const (
	shadowPreferred shadowTier = iota
	shadowBusy
	shadowUnknown
	shadowOverloaded
	shadowTierCount
	shadowReservedPool        = "reserved"
	shadowSharedPool          = "shared"
	shadowReject              = "reject"
	shadowUnavailable         = "unavailable"
	shadowHealth              = "health_exception"
	shadowSame                = "same_choice"
	shadowDifferentReplica    = "different_replica"
	shadowDifferentPool       = "different_pool"
	shadowDifferentAdmission  = "different_admission"
	shadowUnknownMetrics      = "unknown_metrics"
	shadowFirstLatencyBucket  = 0.000001
	shadowLatencyBucketFactor = 10
	shadowLatencyBucketCount  = 6
)

var (
	tierShadowDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "router_tier_shadow_decisions_total", Help: "Actual and hypothetical tier routing outcomes; shadow never dispatches",
	}, []string{"model", "caller_pool", "actual", "proposed", "comparison"})
	tierShadowEvaluationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "router_tier_shadow_evaluation_seconds", Help: "Time spent observing a routing decision with tier shadow enabled",
		Buckets: prometheus.ExponentialBuckets(shadowFirstLatencyBucket, shadowLatencyBucketFactor, shadowLatencyBucketCount),
	}, []string{"model"})
)

// TierShadowObservation describes a decision already made by live routing.
// No value returned by shadow observation can affect dispatch or rejection.
type TierShadowObservation struct {
	Model, OrgID                            string
	Order                                   []string
	Actual                                  *Enclave
	Rejected, Probe, CanProbe, ServeThrough bool
}

type tierShadowDecision struct {
	outcome, host string
	tier          shadowTier
}

func (s *tierShadowMetrics) tier(policy config.TierShadowConfig, now time.Time) shadowTier {
	if policy.MaxRequestsRunning <= 0 && policy.MaxRequestsWaiting <= 0 {
		return shadowPreferred
	}
	if s == nil {
		return shadowUnknown
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sample := s.sample
	if sample.collected.IsZero() || now.Sub(sample.collected) > sampleStalenessLimit ||
		!sample.hasWaiting || policy.MaxRequestsRunning > 0 && !sample.hasRunning {
		return shadowUnknown
	}
	if s.queues[policy] || policy.MaxRequestsWaiting > 0 && sample.waiting >= float64(policy.MaxRequestsWaiting) {
		return shadowOverloaded
	}
	if sample.waiting == 0 && (policy.MaxRequestsRunning <= 0 || sample.running < float64(policy.MaxRequestsRunning)) {
		return shadowPreferred
	}
	return shadowBusy
}

func (m *Model) ObserveTierShadow(observation TierShadowObservation) {
	started := time.Now()
	m.mu.RLock()
	policy := m.TierShadow
	primary, spill := m.sharedHosts, map[string]bool(nil)
	callerPool := shadowSharedPool
	if reserved, ok := m.reservedByOrg[observation.OrgID]; ok {
		primary, spill = reserved, m.sharedHosts
		callerPool = shadowReservedPool
		policy = m.shadowPoliciesByOrg[observation.OrgID]
	}
	if policy == nil {
		m.mu.RUnlock()
		return
	}
	actual := shadowUnavailable
	if observation.Actual != nil {
		actual = shadowSharedPool
		if callerPool == shadowReservedPool && primary[observation.Actual.host] {
			actual = shadowReservedPool
		}
	}
	if observation.Actual != nil && observation.Rejected {
		actual = shadowReject
	}
	decision := tierShadowDecision{outcome: shadowHealth}
	if !observation.Probe && (observation.Actual == nil || observation.Actual.breakerClosed()) {
		decision = m.evaluateTierShadowLocked(observation, *policy, primary, spill, callerPool, started)
	} else {
		actual = shadowHealth
	}
	m.mu.RUnlock()
	comparison := shadowSame
	switch {
	case actual == shadowHealth || decision.outcome == shadowHealth:
		comparison = shadowHealth
	case decision.tier == shadowUnknown:
		comparison = shadowUnknownMetrics
	case actual != decision.outcome:
		comparison = shadowDifferentPool
		if actual == shadowReject || decision.outcome == shadowReject || actual == shadowUnavailable || decision.outcome == shadowUnavailable {
			comparison = shadowDifferentAdmission
		}
	case observation.Actual != nil && decision.host != "" && observation.Actual.host != decision.host:
		comparison = shadowDifferentReplica
	}
	tierShadowDecisions.WithLabelValues(observation.Model, callerPool, actual, decision.outcome, comparison).Inc()
	tierShadowEvaluationSeconds.WithLabelValues(observation.Model).Observe(time.Since(started).Seconds())
}

// evaluateTierShadowLocked reads samples and breaker state without claiming
// probes, consuming the live random picker, or updating cache-affinity state.
func (m *Model) evaluateTierShadowLocked(observation TierShadowObservation, policy config.TierShadowConfig, primary, spill map[string]bool, callerPool string, now time.Time) tierShadowDecision {
	pools := []map[string]bool{primary}
	if len(spill) > 0 {
		pools = append(pools, spill)
	}
	var buckets [shadowTierCount][2][]*Enclave
	var probes [2]bool
	degraded := false
	for pool, allowed := range pools {
		for _, enclave := range m.Enclaves {
			if allowed != nil && !allowed[enclave.host] || !enclave.attestationValid(now) {
				continue
			}
			if !enclave.breakerClosed() {
				degraded = true
				cb := enclave.cb
				probes[pool] = probes[pool] || observation.CanProbe && cb.loadState() == cbOpen && now.Sub(time.Unix(0, cb.lastFailureNano.Load())) >= cbCooldown
				continue
			}
			tier := enclave.tierShadow.tier(policy, now)
			buckets[tier][pool] = append(buckets[tier][pool], enclave)
		}
	}
	for tier, byPool := range buckets {
		for pool, candidates := range byPool {
			if shadowTier(tier) == shadowPreferred && probes[pool] {
				return tierShadowDecision{outcome: shadowHealth}
			}
			if len(candidates) == 0 {
				continue
			}
			if shadowTier(tier) == shadowOverloaded && !observation.ServeThrough {
				return tierShadowDecision{outcome: shadowReject, tier: shadowOverloaded}
			}
			outcome := callerPool
			if pool > 0 {
				outcome = shadowSharedPool
			}
			return tierShadowDecision{outcome: outcome, tier: shadowTier(tier), host: m.shadowPickLocked(observation, candidates).host}
		}
	}
	if degraded {
		return tierShadowDecision{outcome: shadowHealth}
	}
	return tierShadowDecision{outcome: shadowUnavailable}
}

func (m *Model) shadowPickLocked(observation TierShadowObservation, candidates []*Enclave) *Enclave {
	least := candidates[0]
	fallback := least
	for _, enclave := range candidates {
		if enclave.inflight.Load() < least.inflight.Load() || enclave.inflight.Load() == least.inflight.Load() && enclave.host < least.host {
			least = enclave
		}
		if enclave.host < fallback.host {
			fallback = enclave
		}
	}
	for _, host := range observation.Order {
		for _, enclave := range candidates {
			if enclave.host != host {
				continue
			}
			if m.CacheRoute != nil && m.CacheRoute.MaxInflightDelta != nil {
				cap := *m.CacheRoute.MaxInflightDelta
				if cap >= 0 && enclave.inflight.Load()-least.inflight.Load() > int64(cap) {
					return least
				}
			}
			return enclave
		}
	}
	// Random tie breaking has no uniquely predicted host. Count agreement
	// when the actual choice is one of the hypothetical eligible replicas.
	for _, enclave := range candidates {
		if enclave == observation.Actual {
			return enclave
		}
	}
	return fallback
}
