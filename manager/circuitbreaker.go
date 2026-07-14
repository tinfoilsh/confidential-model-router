package manager

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type cbState int32

const (
	cbClosed   cbState = iota // healthy — traffic flows normally
	cbOpen                    // unhealthy — traffic is blocked
	cbHalfOpen                // probing — one request allowed to test recovery
)

const (
	cbFailureThreshold = 3                // consecutive failures before opening
	cbCooldown         = 30 * time.Second // wait before probing after opening
)

// circuitBreaker tracks the health of a single enclave backend using the
// standard closed → open → half-open state machine.
//
// All methods are safe for concurrent use.
type circuitBreaker struct {
	state               atomic.Int32
	consecutiveFailures atomic.Int64
	lastFailureNano     atomic.Int64
	// probeGen counts probe claims; an AbortProbe caller must present the
	// token of the current claim, so a stale holder cannot release a probe
	// it no longer owns.
	probeGen  atomic.Uint64
	retired   atomic.Bool
	publishMu sync.RWMutex
}

func newCircuitBreaker() *circuitBreaker {
	return &circuitBreaker{}
}

func (cb *circuitBreaker) loadState() cbState {
	return cbState(cb.state.Load())
}

func (cb *circuitBreaker) storeState(s cbState) {
	cb.state.Store(int32(s))
}

func (cb *circuitBreaker) casState(old, new cbState) bool {
	return cb.state.CompareAndSwap(int32(old), int32(new))
}

// RecordSuccess resets the circuit breaker to the closed state.
func (cb *circuitBreaker) RecordSuccess() {
	cb.consecutiveFailures.Store(0)
	cb.storeState(cbClosed)
}

// RecordFailure increments the consecutive failure counter and transitions
// to the open state once the threshold is reached.
func (cb *circuitBreaker) RecordFailure() {
	cb.lastFailureNano.Store(time.Now().UnixNano())
	failures := cb.consecutiveFailures.Add(1)
	if failures >= cbFailureThreshold {
		cb.storeState(cbOpen)
	}
}

// Closed reports whether the circuit breaker is in the closed (healthy) state.
func (cb *circuitBreaker) Closed() bool {
	return cb.loadState() == cbClosed
}

// ClaimProbe attempts to transition an open circuit breaker to half-open
// after the cooldown has elapsed. When this caller wins the CAS it returns
// (token, true): exactly one probe request must be dispatched for the claim,
// resolving it via RecordSuccess or RecordFailure — or AbortProbe with the
// token if the probe is cancelled before the backend answers.
func (cb *circuitBreaker) ClaimProbe() (uint64, bool) {
	if cb.loadState() != cbOpen {
		return 0, false
	}
	last := time.Unix(0, cb.lastFailureNano.Load())
	if time.Since(last) < cbCooldown {
		return 0, false
	}
	if !cb.casState(cbOpen, cbHalfOpen) {
		return 0, false
	}
	return cb.probeGen.Add(1), true
}

// AbortProbe returns the claimed recovery probe identified by token to the
// open state without counting a client cancellation as a backend failure.
// A token from a superseded claim is refused, so an unrelated in-flight
// request's cancellation can never release a probe it does not own.
func (cb *circuitBreaker) AbortProbe(token uint64) bool {
	if cb.probeGen.Load() != token || cb.loadState() != cbHalfOpen {
		return false
	}
	// Restart the cooldown before exposing the open state: a ClaimProbe
	// caller that observes cbOpen with the stale pre-probe timestamp would
	// claim a fresh probe with no cooldown at all. A spurious store when
	// the CAS below loses is harmless — the concurrent transition owns the
	// timestamp from then on.
	cb.lastFailureNano.Store(time.Now().UnixNano())
	return cb.casState(cbHalfOpen, cbOpen)
}

// ProbeClaim ties a claimed recovery probe to the one request dispatched
// for it. The request's terminal outcome resolves the breaker; a client
// cancellation calls Abort instead, and only the claim holder may do so.
type ProbeClaim struct {
	cb        *circuitBreaker
	token     uint64
	modelName string
	host      string
}

// Abort returns the claimed probe to the open state and restarts its
// cooldown. Safe to call on a nil claim; reports whether the abort happened.
func (pc *ProbeClaim) Abort() bool {
	if pc == nil || !pc.cb.AbortProbe(pc.token) {
		return false
	}
	publishBreakerState(pc.modelName, pc.host, pc.cb)
	return true
}

// probeClaimContextKey carries the public proxy path's probe claim in the
// request context, so the per-enclave error handler can tell whether a
// cancelled request owns the in-flight probe.
type probeClaimContextKey struct{}

// WithProbeClaim attaches a probe claim to the request context of the one
// request dispatched for it.
func WithProbeClaim(ctx context.Context, claim *ProbeClaim) context.Context {
	return context.WithValue(ctx, probeClaimContextKey{}, claim)
}

// ProbeClaimFromContext returns the request's probe claim, or nil when the
// request does not own one.
func ProbeClaimFromContext(ctx context.Context) *ProbeClaim {
	claim, _ := ctx.Value(probeClaimContextKey{}).(*ProbeClaim)
	return claim
}

// State returns the current state for observability.
func (cb *circuitBreaker) State() cbState {
	return cb.loadState()
}

// ConsecutiveFailures returns the current consecutive failure count.
func (cb *circuitBreaker) ConsecutiveFailures() int64 {
	return cb.consecutiveFailures.Load()
}

// Retire marks the breaker's enclave as removed from the pool and waits for
// any state publication that already observed it as active to finish. Once
// this returns, shutdown can delete the gauge without a draining request
// recreating it afterward.
func (cb *circuitBreaker) Retire() {
	cb.retired.Store(true)
	cb.publishMu.Lock()
	cb.publishMu.Unlock()
}

// Retired reports whether the enclave was removed from the pool.
func (cb *circuitBreaker) Retired() bool { return cb.retired.Load() }

// publishIfActive serializes a state publication with retirement. Holding the
// read lock through publish prevents Retire from returning between the retired
// check and the publication itself. publish must not call Retire.
func (cb *circuitBreaker) publishIfActive(publish func(cbState)) {
	cb.publishMu.RLock()
	defer cb.publishMu.RUnlock()
	if cb.Retired() {
		return
	}
	publish(cb.State())
}
