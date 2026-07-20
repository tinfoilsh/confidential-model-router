package ratelimit

import (
	"sync"
	"time"
)

// RequestTracker tracks request counts and uncached prompt token debits per
// API key per model with fixed one-minute windows. Safe for concurrent use.
type RequestTracker struct {
	mu          sync.Mutex
	buckets     map[string]*bucketEntry // key: "apiKey\x00model"
	nowFunc     func() time.Time
	lastCleanup time.Time
}

type bucketEntry struct {
	count       int64
	tokens      int64
	windowStart time.Time
}

// Totals is the accumulated usage for an API key and model in the current
// one-minute window. WindowStart identifies the window a debit landed in so a
// later refund can be dropped if the window has already rolled over.
type Totals struct {
	Count       int64
	Tokens      int64
	ResetIn     time.Duration
	WindowStart time.Time
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

// Record atomically increments the request count for the API key and model,
// debits tokens from its uncached-prompt-token budget, and returns the
// window's running totals. ResetIn is the time remaining until the window
// resets, always in (0, time.Minute]: a request landing exactly on a minute
// boundary starts the next window. An empty API key is not tracked and
// returns the zero Totals.
func (t *RequestTracker) Record(apiKey, model string, tokens int64) Totals {
	if apiKey == "" {
		return Totals{}
	}

	now := t.nowFunc()
	key := bucketKey(apiKey, model)

	t.mu.Lock()
	defer t.mu.Unlock()

	b, ok := t.buckets[key]
	if !ok {
		b = &bucketEntry{
			count:       1,
			tokens:      tokens,
			windowStart: now.Truncate(time.Minute),
		}
		t.buckets[key] = b
	} else {
		// Reset window if the minute has changed
		windowStart := now.Truncate(time.Minute)
		if windowStart.After(b.windowStart) {
			b.count = 0
			b.tokens = 0
			b.windowStart = windowStart
		}
		b.count++
		b.tokens += tokens
	}

	// Lazy cleanup: at most once per minute
	if now.Sub(t.lastCleanup) >= time.Minute {
		t.cleanup(now)
		t.lastCleanup = now
	}

	return Totals{
		Count:       b.count,
		Tokens:      b.tokens,
		ResetIn:     b.windowStart.Add(time.Minute).Sub(now),
		WindowStart: b.windowStart,
	}
}

// RefundTokens returns unused debited tokens to the window they were debited
// from, identified by windowStart. A refund whose window has already rolled
// over is dropped: the debit it would offset has expired with the window, and
// crediting the current window would let the caller overdraw it.
func (t *RequestTracker) RefundTokens(apiKey, model string, tokens int64, windowStart time.Time) {
	if apiKey == "" || tokens <= 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	b, ok := t.buckets[bucketKey(apiKey, model)]
	if !ok || !b.windowStart.Equal(windowStart) {
		return
	}
	b.tokens = max(0, b.tokens-tokens)
}

// cleanup removes entries whose windows are more than 2 minutes old.
// Must be called with t.mu held.
func (t *RequestTracker) cleanup(now time.Time) {
	cutoff := now.Add(-2 * time.Minute)
	for key, b := range t.buckets {
		if b.windowStart.Before(cutoff) {
			delete(t.buckets, key)
		}
	}
}
