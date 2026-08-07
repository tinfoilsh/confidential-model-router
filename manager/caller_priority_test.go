package manager

import (
	"context"
	"testing"
)

// TestCallerPriorityContext pins the context round-trip: only "configured"
// is worth carrying, and every other state — absent, empty, explicit
// "none" — reads back as "none".
func TestCallerPriorityContext(t *testing.T) {
	ctx := context.Background()

	if got := CallerPriorityFromContext(ctx); got != "none" {
		t.Fatalf("absent = %q, want none", got)
	}
	if got := CallerPriorityFromContext(WithCallerPriority(ctx, "")); got != "none" {
		t.Fatalf("empty = %q, want none", got)
	}
	if got := CallerPriorityFromContext(WithCallerPriority(ctx, "none")); got != "none" {
		t.Fatalf("none = %q, want none", got)
	}
	if got := CallerPriorityFromContext(WithCallerPriority(ctx, "configured")); got != "configured" {
		t.Fatalf("configured = %q, want configured", got)
	}
}
