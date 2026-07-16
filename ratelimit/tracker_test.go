package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestRecordCounts(t *testing.T) {
	tracker := NewRequestTracker()

	for i := int64(1); i <= 4; i++ {
		if got, _ := tracker.Record("key1", "model1"); got != i {
			t.Fatalf("expected count %d, got %d", i, got)
		}
	}
}

func TestDifferentKeysAreIndependent(t *testing.T) {
	tracker := NewRequestTracker()

	for i := 0; i < 5; i++ {
		tracker.Record("key1", "model1")
	}

	if got, _ := tracker.Record("key1", "model1"); got != 6 {
		t.Fatalf("key1/model1 expected count 6, got %d", got)
	}
	if got, _ := tracker.Record("key2", "model1"); got != 1 {
		t.Fatalf("key2/model1 expected count 1, got %d", got)
	}
	if got, _ := tracker.Record("key1", "model2"); got != 1 {
		t.Fatalf("key1/model2 expected count 1, got %d", got)
	}
}

func TestWindowReset(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	for i := 0; i < 5; i++ {
		tracker.Record("key1", "model1")
	}
	if got, _ := tracker.Record("key1", "model1"); got != 6 {
		t.Fatalf("expected count 6 in current window, got %d", got)
	}

	// Advance to next minute
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	if got, _ := tracker.Record("key1", "model1"); got != 1 {
		t.Fatalf("expected count 1 after window reset, got %d", got)
	}
}

func TestResetDuration(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 15, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	if _, resetIn := tracker.Record("key1", "model1"); resetIn != 45*time.Second {
		t.Fatalf("expected reset in 45s, got %v", resetIn)
	}

	now = time.Date(2025, 1, 1, 10, 30, 59, 5e8, time.UTC)
	if _, resetIn := tracker.Record("key1", "model1"); resetIn != 500*time.Millisecond {
		t.Fatalf("expected reset in 500ms, got %v", resetIn)
	}

	// A fresh window has the full minute remaining
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	if _, resetIn := tracker.Record("key1", "model1"); resetIn != time.Minute {
		t.Fatalf("expected reset in 1m for fresh window, got %v", resetIn)
	}
}

func TestEmptyAPIKeyIgnored(t *testing.T) {
	tracker := NewRequestTracker()

	if got, _ := tracker.Record("", "model1"); got != 0 {
		t.Fatalf("empty API key should not be tracked, got count %d", got)
	}
}

func TestCleanup(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	tracker.Record("key1", "model1")

	// Advance 3 minutes and trigger cleanup via Record
	now = time.Date(2025, 1, 1, 10, 3, 1, 0, time.UTC)
	tracker.Record("key2", "model1") // triggers cleanup

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
			tracker.Record("key1", "model1")
		}()
	}
	wg.Wait()

	if got, _ := tracker.Record("key1", "model1"); got != 101 {
		t.Fatalf("expected count 101 after concurrent records, got %d", got)
	}
}
