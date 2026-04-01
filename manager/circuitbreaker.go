package manager

import (
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

// NeedProbe attempts to transition an open circuit breaker to half-open after
// the cooldown has elapsed. Returns true if this caller won the CAS and should
// send exactly one probe request to this enclave.
func (cb *circuitBreaker) NeedProbe() bool {
	if cb.loadState() != cbOpen {
		return false
	}
	last := time.Unix(0, cb.lastFailureNano.Load())
	if time.Since(last) < cbCooldown {
		return false
	}
	return cb.casState(cbOpen, cbHalfOpen)
}

// State returns the current state for observability.
func (cb *circuitBreaker) State() cbState {
	return cb.loadState()
}

// ConsecutiveFailures returns the current consecutive failure count.
func (cb *circuitBreaker) ConsecutiveFailures() int64 {
	return cb.consecutiveFailures.Load()
}
