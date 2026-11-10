package manager

import (
	"math/rand/v2"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
)

type routingTier int

const (
	tierPreferred routingTier = iota
	tierBusy
	tierUnknown
	tierOverloaded
	tierCount
)

// spillPoliciesLocked keeps shared metrics available even if the shared
// pool's own guard is disabled. The caller must hold m.mu.
func (m *Model) spillPoliciesLocked(host string) []*config.OverloadConfig {
	if !m.sharedHosts[host] {
		return nil
	}
	var policies []*config.OverloadConfig
	for _, policy := range m.policiesByCaller {
		if policy.Enabled() {
			policies = append(policies, policy)
		}
	}
	return policies
}

// localInflight is the caller's own dispatch count for the enclave: an
// exact, immediately-updated signal, unlike the polled counts which are a
// whole sample stale and blind to a burst that arrives inside one poll
// interval. It is per router, so with N replicas the instantaneous cap is
// N × soft_max until the global polled count corrects it.
func (m *enclaveMetrics) tier(policy *config.OverloadConfig, localInflight int64) (routingTier, float64) {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	if !policy.Enabled() {
		return tierPreferred, 0
	}
	counts := m.latestCounts
	if policy.SoftMaxRequestsRunning > 0 {
		if local := float64(localInflight); local > counts.running {
			counts.running = local
		}
		// An at-cap local count outranks a missing sample: without this,
		// dead polling pins every caller to its first pool, inert to
		// soft_max, because unknown competes with unknown on pool order.
		// Overloaded (not busy) so unknown spill candidates win; when
		// every usable pool is at its local cap with no samples, the
		// usual tier-2 503 applies instead of unbounded queueing.
		if localInflight >= int64(policy.SoftMaxRequestsRunning) {
			if m.latestCollected.IsZero() || time.Since(m.latestCollected) > sampleStalenessLimit ||
				!counts.supports(policy) {
				return tierOverloaded, counts.waiting
			}
		}
	}
	if m.latestCollected.IsZero() || time.Since(m.latestCollected) > sampleStalenessLimit ||
		!counts.supports(policy) {
		return tierUnknown, counts.waiting
	}
	trip, clear := policy.Marks()
	marks := queueMarks{trip, clear}
	if trip > 0 && marks.overloaded(counts.waiting, m.queueOverloads[marks]) {
		return tierOverloaded, counts.waiting
	}
	if counts.waiting == 0 && (policy.SoftMaxRequestsRunning <= 0 || counts.running < float64(policy.SoftMaxRequestsRunning)) {
		return tierPreferred, counts.waiting
	}
	return tierBusy, counts.waiting
}

// queueHeadroom reports whether running plus waiting requests stay below the
// soft max.
func (m *enclaveMetrics) queueHeadroom(policy *config.OverloadConfig, localInflight int64) bool {
	if !policy.Enabled() || policy.SoftMaxRequestsRunning <= 0 {
		return false
	}
	counts, _ := m.latestSample()
	return max(counts.running+counts.waiting, float64(localInflight)) < float64(policy.SoftMaxRequestsRunning)
}

// SelectServing ranks closed backends by the request's tier, then pool, then
// affinity, except that primary hosts with queue headroom precede spill tier
// zero. Unknown load fails open only after known usable capacity. Due
// recovery probes and the all-breakers-open last resort retain their bypass.
// A non-nil enclave reserves one local in-flight request until ReleaseDispatch.
func (m *Model) SelectServing(order []string, claimProbes bool, primary, spill map[string]bool, policy *config.OverloadConfig) (*Enclave, *ProbeClaim, bool, time.Duration, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	enclave, claim, overloaded, retryAfter, waiting := m.selectServingLocked(order, claimProbes, primary, spill, policy, "")
	if enclave != nil {
		enclave.inflight.Add(1)
		BackendInflight.WithLabelValues(enclave.modelName, enclave.host).Inc()
	}
	return enclave, claim, overloaded, retryAfter, waiting
}

// SelectRetry excludes recovery probes, unknown load, and overloaded replicas.
func (m *Model) SelectRetry(failedHost string, order []string, primary, spill map[string]bool, policy *config.OverloadConfig) *Enclave {
	m.mu.Lock()
	defer m.mu.Unlock()
	enclave, _, overloaded, _, _ := m.selectServingLocked(order, false, primary, spill, policy, failedHost)
	if enclave == nil || overloaded {
		return nil
	}
	enclave.inflight.Add(1)
	BackendInflight.WithLabelValues(enclave.modelName, enclave.host).Inc()
	return enclave
}

func (m *Model) selectServingLocked(order []string, claimProbes bool, primary, spill map[string]bool, policy *config.OverloadConfig, excludedHost string) (*Enclave, *ProbeClaim, bool, time.Duration, float64) {
	pools := []map[string]bool{primary}
	if len(spill) > 0 {
		pools = append(pools, spill)
	}
	var buckets [tierCount][2][]*Enclave
	// A short queue during long prefills does not mean the replica is full.
	var queued []*Enclave
	var degraded []*Enclave
	var probes [2][]*Enclave
	waiting := make(map[*Enclave]float64)
	for pool, allowed := range pools {
		for _, enclave := range m.Enclaves {
			if (excludedHost != "" && enclave.host == excludedHost) || (allowed != nil && !allowed[enclave.host]) || !enclave.attestationValid(time.Now()) {
				continue
			}
			if !enclave.breakerClosed() {
				if excludedHost != "" {
					continue
				}
				probes[pool] = append(probes[pool], enclave)
				degraded = append(degraded, enclave)
				continue
			}
			tier := tierUnknown
			if enclave.metrics != nil {
				tier, waiting[enclave] = enclave.metrics.tier(policy, enclave.inflight.Load())
			}
			if excludedHost != "" && tier == tierUnknown {
				continue
			}
			if tier == tierBusy && pool == 0 && len(spill) > 0 && enclave.metrics.queueHeadroom(policy, enclave.inflight.Load()) {
				queued = append(queued, enclave)
				continue
			}
			buckets[tier][pool] = append(buckets[tier][pool], enclave)
		}
	}
	for tier, byPool := range buckets {
		for pool, candidates := range byPool {
			if routingTier(tier) == tierPreferred && claimProbes {
				for _, enclave := range probes[pool] {
					if claim := enclave.claimProbe(); claim != nil {
						return enclave, claim, false, 0, 0
					}
				}
			}
			if routingTier(tier) == tierPreferred && pool == 0 && len(candidates) == 0 {
				candidates = queued
			}
			if len(candidates) == 0 {
				continue
			}
			enclave := m.pickTierLocked(order, candidates)
			if routingTier(tier) == tierOverloaded {
				return enclave, nil, true, policy.RetryAfter(), waiting[enclave]
			}
			if routingTier(tier) == tierUnknown {
				OverloadFailOpenTotal.WithLabelValues(enclave.modelName, enclave.host).Inc()
			}
			return enclave, nil, false, 0, waiting[enclave]
		}
	}
	if len(degraded) > 0 {
		return degraded[0], nil, false, 0, 0
	}
	return nil, nil, false, 0, 0
}

func (m *Model) pickTierLocked(order []string, candidates []*Enclave) *Enclave {
	fallback := candidates[rand.IntN(len(candidates))]
	least := fallback
	for _, enclave := range candidates {
		if enclave.inflight.Load() < least.inflight.Load() {
			least = enclave
		}
	}
	for _, host := range order {
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
	return fallback
}
