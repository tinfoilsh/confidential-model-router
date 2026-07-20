package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestRecordCounts(t *testing.T) {
	tracker := NewRequestTracker()

	for i := int64(1); i <= 4; i++ {
		if got := tracker.Record("key1", "model1", 0); got.Count != i {
			t.Fatalf("expected count %d, got %d", i, got.Count)
		}
	}
}

func TestDifferentKeysAreIndependent(t *testing.T) {
	tracker := NewRequestTracker()

	for i := 0; i < 5; i++ {
		tracker.Record("key1", "model1", 10)
	}

	if got := tracker.Record("key1", "model1", 10); got.Count != 6 || got.Tokens != 60 {
		t.Fatalf("key1/model1 expected count 6 tokens 60, got %d/%d", got.Count, got.Tokens)
	}
	if got := tracker.Record("key2", "model1", 10); got.Count != 1 || got.Tokens != 10 {
		t.Fatalf("key2/model1 expected count 1 tokens 10, got %d/%d", got.Count, got.Tokens)
	}
	if got := tracker.Record("key1", "model2", 10); got.Count != 1 || got.Tokens != 10 {
		t.Fatalf("key1/model2 expected count 1 tokens 10, got %d/%d", got.Count, got.Tokens)
	}
}

func TestWindowReset(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	for i := 0; i < 5; i++ {
		tracker.Record("key1", "model1", 100)
	}
	if got := tracker.Record("key1", "model1", 100); got.Count != 6 || got.Tokens != 600 {
		t.Fatalf("expected count 6 tokens 600 in current window, got %d/%d", got.Count, got.Tokens)
	}

	// Advance to next minute
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	if got := tracker.Record("key1", "model1", 100); got.Count != 1 || got.Tokens != 100 {
		t.Fatalf("expected count 1 tokens 100 after window reset, got %d/%d", got.Count, got.Tokens)
	}
}

func TestResetDuration(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 15, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	if got := tracker.Record("key1", "model1", 0); got.ResetIn != 45*time.Second {
		t.Fatalf("expected reset in 45s, got %v", got.ResetIn)
	}

	now = time.Date(2025, 1, 1, 10, 30, 59, 5e8, time.UTC)
	if got := tracker.Record("key1", "model1", 0); got.ResetIn != 500*time.Millisecond {
		t.Fatalf("expected reset in 500ms, got %v", got.ResetIn)
	}

	// A fresh window has the full minute remaining
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	if got := tracker.Record("key1", "model1", 0); got.ResetIn != time.Minute {
		t.Fatalf("expected reset in 1m for fresh window, got %v", got.ResetIn)
	}
}

func TestEmptyAPIKeyIgnored(t *testing.T) {
	tracker := NewRequestTracker()

	if got := tracker.Record("", "model1", 50); got != (Totals{}) {
		t.Fatalf("empty API key should not be tracked, got %+v", got)
	}
}

func TestRefundTokens(t *testing.T) {
	tracker := NewRequestTracker()

	charged := tracker.Record("key1", "model1", 1000)
	if charged.Tokens != 1000 {
		t.Fatalf("expected tokens 1000, got %d", charged.Tokens)
	}

	// Engine reported 900 of the 1000 estimated tokens were cached
	tracker.RefundTokens("key1", "model1", 900, charged.WindowStart)
	if got := tracker.Record("key1", "model1", 0); got.Tokens != 100 {
		t.Fatalf("expected tokens 100 after refund, got %d", got.Tokens)
	}
}

func TestRefundClampsAtZero(t *testing.T) {
	tracker := NewRequestTracker()

	charged := tracker.Record("key1", "model1", 100)
	tracker.RefundTokens("key1", "model1", 500, charged.WindowStart)
	if got := tracker.Record("key1", "model1", 0); got.Tokens != 0 {
		t.Fatalf("expected tokens clamped to 0, got %d", got.Tokens)
	}
}

func TestRefundAfterWindowRollIsDropped(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	charged := tracker.Record("key1", "model1", 1000)

	// Roll to the next window and accrue new usage
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	tracker.Record("key1", "model1", 300)

	// A refund for the expired window must not credit the current one
	tracker.RefundTokens("key1", "model1", 1000, charged.WindowStart)
	if got := tracker.Record("key1", "model1", 0); got.Tokens != 300 {
		t.Fatalf("expected tokens 300 after stale refund dropped, got %d", got.Tokens)
	}
}

func TestCleanup(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	tracker.Record("key1", "model1", 0)

	// Advance 3 minutes and trigger cleanup via Record
	now = time.Date(2025, 1, 1, 10, 3, 1, 0, time.UTC)
	tracker.Record("key2", "model1", 0) // triggers cleanup

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
			tracker.Record("key1", "model1", 7)
		}()
	}
	wg.Wait()

	if got := tracker.Record("key1", "model1", 0); got.Count != 101 || got.Tokens != 700 {
		t.Fatalf("expected count 101 tokens 700 after concurrent records, got %d/%d", got.Count, got.Tokens)
	}
}
