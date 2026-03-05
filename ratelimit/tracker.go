package ratelimit

import (
	"sync"
	"time"
)

// RequestTracker tracks request counts per API key per model with
// fixed one-minute windows. Safe for concurrent use.
type RequestTracker struct {
	mu          sync.Mutex
	buckets     map[string]*bucketEntry // key: "apiKey\x00model"
	nowFunc     func() time.Time
	lastCleanup time.Time
}

type bucketEntry struct {
	count       int64
	windowStart time.Time
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

// RecordAndCheck atomically increments the request count and returns true
// if the API key has exceeded the limit for the given model in the current
// one-minute window.
func (t *RequestTracker) RecordAndCheck(apiKey, model string, limit int64) bool {
	if apiKey == "" {
		return false
	}

	now := t.nowFunc()
	key := bucketKey(apiKey, model)

	t.mu.Lock()
	defer t.mu.Unlock()

	b, ok := t.buckets[key]
	if !ok {
		b = &bucketEntry{
			count:       1,
			windowStart: now.Truncate(time.Minute),
		}
		t.buckets[key] = b
	} else {
		// Reset window if the minute has changed
		windowStart := now.Truncate(time.Minute)
		if windowStart.After(b.windowStart) {
			b.count = 0
			b.windowStart = windowStart
		}
		b.count++
	}

	// Lazy cleanup: at most once per minute
	if now.Sub(t.lastCleanup) >= time.Minute {
		t.cleanup(now)
		t.lastCleanup = now
	}

	if limit <= 0 {
		return false
	}
	return b.count >= limit
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
