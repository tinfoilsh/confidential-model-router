package ratelimit

import (
	"sync"
	"time"
)

// RequestTracker tracks request counts and token debits per API key per
// model. Safe for concurrent use.
type RequestTracker struct {
	mu          sync.Mutex
	buckets     map[string]*bucketEntry // key: "apiKey\x00model"
	nowFunc     func() time.Time
	lastCleanup time.Time
}

type bucketEntry struct {
	count            int64
	countWindowStart time.Time

	tokens           int64
	tokenWindow      time.Duration
	tokenWindowStart time.Time
}

// Totals is the accumulated usage for an API key and model, per axis, along with window.
type Totals struct {
	Count             int64
	CountResetIn      time.Duration
	Tokens            int64
	TokensResetIn     time.Duration
	TokensWindowStart time.Time
}

// Option configures a RequestTracker.
type Option func(*RequestTracker)

// WithNowFunc injects a custom clock (for testing).
func WithNowFunc(f func() time.Time) Option {
	return func(t *RequestTracker) {
		t.nowFunc = f
	}
}

// NewRequestTracker creates a new RequestTracker.
func NewRequestTracker(opts ...Option) *RequestTracker {
	t := &RequestTracker{
		buckets: make(map[string]*bucketEntry),
		nowFunc: time.Now,
	}
	for _, o := range opts {
		o(t)
	}
	t.lastCleanup = t.nowFunc()
	return t
}

func bucketKey(apiKey, model string) string {
	return apiKey + "\x00" + model
}

// roll resets a window's accumulator if the window has changed. A request
// landing exactly on a window boundary starts the next window.
func roll(now time.Time, window time.Duration, windowStart *time.Time, acc *int64) {
	start := now.Truncate(window)
	if start.After(*windowStart) {
		*acc = 0
		*windowStart = start
	}
}

// Record atomically increments the request count for the API key and model,
// debits tokens from its uncached-prompt-token budget, and returns the
// running totals. Request counts roll per minute; token debits roll per
// tokenWindow, and changing it mid-flight starts the token window fresh. An
// empty API key is not tracked and returns the zero Totals.
func (t *RequestTracker) Record(apiKey, model string, tokens int64, tokenWindow time.Duration) Totals {
	if apiKey == "" {
		return Totals{}
	}

	now := t.nowFunc()
	key := bucketKey(apiKey, model)

	t.mu.Lock()
	defer t.mu.Unlock()

	b, ok := t.buckets[key]
	if !ok || b.tokenWindow != tokenWindow {
		b = &bucketEntry{
			countWindowStart: now.Truncate(time.Minute),
			tokenWindow:      tokenWindow,
			tokenWindowStart: now.Truncate(tokenWindow),
		}
		t.buckets[key] = b
	} else {
		roll(now, time.Minute, &b.countWindowStart, &b.count)
		roll(now, tokenWindow, &b.tokenWindowStart, &b.tokens)
	}
	b.count++
	b.tokens += tokens

	// Lazy cleanup: at most once per minute
	if now.Sub(t.lastCleanup) >= time.Minute {
		t.cleanup(now)
		t.lastCleanup = now
	}

	return Totals{
		Count:             b.count,
		CountResetIn:      b.countWindowStart.Add(time.Minute).Sub(now),
		Tokens:            b.tokens,
		TokensResetIn:     b.tokenWindowStart.Add(tokenWindow).Sub(now),
		TokensWindowStart: b.tokenWindowStart,
	}
}

// ReconcileTokens adjusts the window a debit landed in, identified by
// windowStart, to the engine-reported usage: negative delta refunds an
// over-estimate, positive delta charges the shortfall of an under-estimate.
// A reconciliation whose window has already rolled over is dropped: the
// debit it would correct has expired with the window.
func (t *RequestTracker) ReconcileTokens(apiKey, model string, delta int64, windowStart time.Time) {
	if apiKey == "" || delta == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	b, ok := t.buckets[bucketKey(apiKey, model)]
	if !ok || !b.tokenWindowStart.Equal(windowStart) {
		return
	}
	b.tokens = max(0, b.tokens+delta)
}

// cleanup removes entries whose windows have both been stale for at least a
// full window length. Must be called with t.mu held.
func (t *RequestTracker) cleanup(now time.Time) {
	for key, b := range t.buckets {
		if now.After(b.countWindowStart.Add(2*time.Minute)) &&
			now.After(b.tokenWindowStart.Add(2*b.tokenWindow)) {
			delete(t.buckets, key)
		}
	}
}
