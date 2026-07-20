package ratelimit

import (
	"sync"
	"testing"
	"time"
)

const minute = time.Minute

func TestRecordCounts(t *testing.T) {
	tracker := NewRequestTracker()

	for i := int64(1); i <= 4; i++ {
		if got := tracker.Record("key1", "model1", 0, minute); got.Count != i {
			t.Fatalf("expected count %d, got %d", i, got.Count)
		}
	}
}

func TestDifferentKeysAreIndependent(t *testing.T) {
	tracker := NewRequestTracker()

	for i := 0; i < 5; i++ {
		tracker.Record("key1", "model1", 10, minute)
	}

	if got := tracker.Record("key1", "model1", 10, minute); got.Count != 6 || got.Tokens != 60 {
		t.Fatalf("key1/model1 expected count 6 tokens 60, got %d/%d", got.Count, got.Tokens)
	}
	if got := tracker.Record("key2", "model1", 10, minute); got.Count != 1 || got.Tokens != 10 {
		t.Fatalf("key2/model1 expected count 1 tokens 10, got %d/%d", got.Count, got.Tokens)
	}
	if got := tracker.Record("key1", "model2", 10, minute); got.Count != 1 || got.Tokens != 10 {
		t.Fatalf("key1/model2 expected count 1 tokens 10, got %d/%d", got.Count, got.Tokens)
	}
}

func TestWindowReset(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	for i := 0; i < 5; i++ {
		tracker.Record("key1", "model1", 100, minute)
	}
	if got := tracker.Record("key1", "model1", 100, minute); got.Count != 6 || got.Tokens != 600 {
		t.Fatalf("expected count 6 tokens 600 in current window, got %d/%d", got.Count, got.Tokens)
	}

	// Advance to next minute
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	if got := tracker.Record("key1", "model1", 100, minute); got.Count != 1 || got.Tokens != 100 {
		t.Fatalf("expected count 1 tokens 100 after window reset, got %d/%d", got.Count, got.Tokens)
	}
}

func TestIndependentWindows(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	tracker.Record("key1", "model1", 100, 5*minute)
	tracker.Record("key1", "model1", 100, 5*minute)

	// Next minute: count window rolls, token window does not
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	got := tracker.Record("key1", "model1", 100, 5*minute)
	if got.Count != 1 || got.Tokens != 300 {
		t.Fatalf("expected count 1 tokens 300, got %d/%d", got.Count, got.Tokens)
	}
	if got.CountResetIn != minute || got.TokensResetIn != 4*minute {
		t.Fatalf("expected resets 1m/4m, got %v/%v", got.CountResetIn, got.TokensResetIn)
	}

	// Past the 5-minute boundary: token window rolls too
	now = time.Date(2025, 1, 1, 10, 35, 0, 0, time.UTC)
	if got := tracker.Record("key1", "model1", 100, 5*minute); got.Tokens != 100 {
		t.Fatalf("expected tokens 100 after token window reset, got %d", got.Tokens)
	}
}

func TestResetDuration(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 15, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	if got := tracker.Record("key1", "model1", 0, minute); got.CountResetIn != 45*time.Second {
		t.Fatalf("expected reset in 45s, got %v", got.CountResetIn)
	}

	now = time.Date(2025, 1, 1, 10, 30, 59, 5e8, time.UTC)
	if got := tracker.Record("key1", "model1", 0, minute); got.CountResetIn != 500*time.Millisecond {
		t.Fatalf("expected reset in 500ms, got %v", got.CountResetIn)
	}

	// A fresh window has the full minute remaining
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	if got := tracker.Record("key1", "model1", 0, minute); got.CountResetIn != time.Minute {
		t.Fatalf("expected reset in 1m for fresh window, got %v", got.CountResetIn)
	}
}

func TestEmptyAPIKeyIgnored(t *testing.T) {
	tracker := NewRequestTracker()

	if got := tracker.Record("", "model1", 50, minute); got != (Totals{}) {
		t.Fatalf("empty API key should not be tracked, got %+v", got)
	}
}

func TestReconcileRefundsOverEstimate(t *testing.T) {
	tracker := NewRequestTracker()

	charged := tracker.Record("key1", "model1", 1000, minute)
	if charged.Tokens != 1000 {
		t.Fatalf("expected tokens 1000, got %d", charged.Tokens)
	}

	// Engine reported only 100 of the 1000 estimated tokens were uncached
	tracker.ReconcileTokens("key1", "model1", -900, charged.TokensWindowStart)
	if got := tracker.Record("key1", "model1", 0, minute); got.Tokens != 100 {
		t.Fatalf("expected tokens 100 after refund, got %d", got.Tokens)
	}
}

func TestReconcileChargesUnderEstimate(t *testing.T) {
	tracker := NewRequestTracker()

	// Estimated 1000 but the engine reported 5000 uncached
	charged := tracker.Record("key1", "model1", 1000, minute)
	tracker.ReconcileTokens("key1", "model1", 4000, charged.TokensWindowStart)
	if got := tracker.Record("key1", "model1", 0, minute); got.Tokens != 5000 {
		t.Fatalf("expected tokens 5000 after shortfall charged, got %d", got.Tokens)
	}
}

func TestReconcileClampsAtZero(t *testing.T) {
	tracker := NewRequestTracker()

	charged := tracker.Record("key1", "model1", 100, minute)
	tracker.ReconcileTokens("key1", "model1", -500, charged.TokensWindowStart)
	if got := tracker.Record("key1", "model1", 0, minute); got.Tokens != 0 {
		t.Fatalf("expected tokens clamped to 0, got %d", got.Tokens)
	}
}

func TestReconcileAfterWindowRollIsDropped(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 30, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	charged := tracker.Record("key1", "model1", 1000, minute)

	// Roll to the next window and accrue new usage
	now = time.Date(2025, 1, 1, 10, 31, 0, 0, time.UTC)
	tracker.Record("key1", "model1", 300, minute)

	// A reconciliation for the expired window must not touch the current one
	tracker.ReconcileTokens("key1", "model1", -1000, charged.TokensWindowStart)
	if got := tracker.Record("key1", "model1", 0, minute); got.Tokens != 300 {
		t.Fatalf("expected tokens 300 after stale reconciliation dropped, got %d", got.Tokens)
	}
}

func TestCleanup(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	tracker.Record("key1", "model1", 0, minute)

	// Advance 3 minutes and trigger cleanup via Record
	now = time.Date(2025, 1, 1, 10, 3, 1, 0, time.UTC)
	tracker.Record("key2", "model1", 0, minute) // triggers cleanup

	tracker.mu.Lock()
	_, exists := tracker.buckets[bucketKey("key1", "model1")]
	tracker.mu.Unlock()
	if exists {
		t.Fatal("stale entry should have been cleaned up")
	}
}

func TestCleanupSparesLongTokenWindows(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	tracker := NewRequestTracker(WithNowFunc(func() time.Time { return now }))

	tracker.Record("key1", "model1", 500, 10*minute)

	// 3 minutes on: the count window is stale but the token window is live
	now = time.Date(2025, 1, 1, 10, 3, 1, 0, time.UTC)
	tracker.Record("key2", "model1", 0, minute) // triggers cleanup

	if got := tracker.Record("key1", "model1", 0, 10*minute); got.Tokens != 500 {
		t.Fatalf("expected live token window to survive cleanup with tokens 500, got %d", got.Tokens)
	}
}

func TestConcurrentAccess(t *testing.T) {
	tracker := NewRequestTracker()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.Record("key1", "model1", 7, minute)
		}()
	}
	wg.Wait()

	if got := tracker.Record("key1", "model1", 0, minute); got.Count != 101 || got.Tokens != 700 {
		t.Fatalf("expected count 101 tokens 700 after concurrent records, got %d/%d", got.Count, got.Tokens)
	}
}
