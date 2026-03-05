package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestRecordAndCheck(t *testing.T) {
	tracker := NewRequestTracker()

	// First two requests under limit of 3
	if tracker.RecordAndCheck("key1", "model1", 3) {
		t.Fatal("should not be rate limited at 1/3")
	}
	if tracker.RecordAndCheck("key1", "model1", 3) {
		t.Fatal("should not be rate limited at 2/3")
	}

	// Third request hits limit
	if !tracker.RecordAndCheck("key1", "model1", 3) {
		t.Fatal("should be rate limited at 3/3")
	}

	// Over limit
	if !tracker.RecordAndCheck("key1", "model1", 3) {
		t.Fatal("should be rate limited at 4/3")
	}
}

func TestDifferentKeysAreIndependent(t *testing.T) {
	tracker := NewRequestTracker()

	for i := 0; i < 5; i++ {
		tracker.RecordAndCheck("key1", "model1", 100)
	}
	tracker.RecordAndCheck("key2", "model1", 100)
	tracker.RecordAndCheck("key1", "model2", 100)

	// key1/model1 has 5 requests
	if !tracker.RecordAndCheck("key1", "model1", 3) {
		t.Fatal("key1/model1 should be rate limited")
	}
	// key2/model1 has 1 request
	if tracker.RecordAndCheck("key2", "model1", 3) {
		t.Fatal("key2/model1 should not be rate limited")
	}
	// key1/model2 has 1 request
	if tracker.RecordAndCheck("key1", "model2", 3) {
		t.Fatal("key1/model2 should not be rate limited")
	}
}

func TestWindowReset(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	for i := 0; i < 5; i++ {
		tracker.RecordAndCheck("key1", "model1", 100)
	}
	if !tracker.RecordAndCheck("key1", "model1", 3) {
		t.Fatal("should be rate limited in current window")
	}

	// Advance to next minute
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	if tracker.RecordAndCheck("key1", "model1", 3) {
		t.Fatal("should not be rate limited after window reset")
	}
}

func TestEmptyAPIKeyIgnored(t *testing.T) {
	tracker := NewRequestTracker()

	if tracker.RecordAndCheck("", "model1", 1) {
		t.Fatal("empty API key should never be rate limited")
	}
}

func TestZeroLimitNeverRateLimits(t *testing.T) {
	tracker := NewRequestTracker()
	for i := 0; i < 100; i++ {
		tracker.RecordAndCheck("key1", "model1", 100)
	}

	if tracker.RecordAndCheck("key1", "model1", 0) {
		t.Fatal("zero limit should never rate limit")
	}
	if tracker.RecordAndCheck("key1", "model1", -1) {
		t.Fatal("negative limit should never rate limit")
	}
}

func TestCleanup(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	tracker.RecordAndCheck("key1", "model1", 100)

	// Advance 3 minutes and trigger cleanup via RecordAndCheck
	now = time.Date(2025, 1, 1, 10, 3, 1, 0, time.UTC)
	tracker.RecordAndCheck("key2", "model1", 100) // triggers cleanup

	tracker.mu.Lock()
	_, exists := tracker.buckets[bucketKey("key1", "model1")]
	tracker.mu.Unlock()
	if exists {
		t.Fatal("stale entry should have been cleaned up")
	}
}

func TestConcurrentAccess(t *testing.T) {
	tracker := NewRequestTracker()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordAndCheck("key1", "model1", 100)
		}()
	}
	wg.Wait()
}
