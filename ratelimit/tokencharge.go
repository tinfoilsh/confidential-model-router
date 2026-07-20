package ratelimit

import (
	"context"
	"time"
)

// TokenCharge records the pessimistic uncached-prompt-token debit made for a
// request at admission time (sized as if nothing were cached). It travels on
// the request context so the proxy can refund the cached portion once the
// engine reports actual usage.
type TokenCharge struct {
	ID          string
	Model       string
	Tokens      int64
	WindowStart time.Time
}

type tokenChargeKey struct{}

// WithTokenCharge returns a context carrying the admission-time token charge.
func WithTokenCharge(ctx context.Context, c TokenCharge) context.Context {
	return context.WithValue(ctx, tokenChargeKey{}, c)
}

// TokenChargeFromContext returns the admission-time token charge, if any.
func TokenChargeFromContext(ctx context.Context) (TokenCharge, bool) {
	c, ok := ctx.Value(tokenChargeKey{}).(TokenCharge)
	return c, ok
}
