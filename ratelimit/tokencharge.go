package ratelimit

import (
	"context"
	"sync/atomic"
	"time"
)

// TokenCharge records the pessimistic uncached-prompt-token debit made for a
// request at admission time (sized as if nothing were cached). It travels on
// the request context so whichever path serves the request — plain proxy or
// tool loop — can reconcile the debit against engine-reported usage.
type TokenCharge struct {
	ID          string
	Model       string
	Tokens      int64
	WindowStart time.Time
}

type tokenChargeKey struct{}

type tokenChargeBox struct {
	charge   TokenCharge
	consumed atomic.Bool
}

// WithTokenCharge returns a context carrying the admission-time token charge.
func WithTokenCharge(ctx context.Context, c TokenCharge) context.Context {
	return context.WithValue(ctx, tokenChargeKey{}, &tokenChargeBox{charge: c})
}

// ConsumeTokenCharge returns the admission-time token charge and marks it
// consumed. Only the first caller receives it, so a charge is reconciled at
// most once even when several paths observe the same request.
func ConsumeTokenCharge(ctx context.Context) (TokenCharge, bool) {
	box, ok := ctx.Value(tokenChargeKey{}).(*tokenChargeBox)
	if !ok || !box.consumed.CompareAndSwap(false, true) {
		return TokenCharge{}, false
	}
	return box.charge, true
}
