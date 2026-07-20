package ratelimit

import (
	"context"
	"time"
)

// Charge records the pessimistic uncached-prompt-token debit made for a
// request at admission time (sized as if nothing were cached). It travels on
// the request context so the proxy can refund the cached portion once the
// engine reports actual usage.
type Charge struct {
	ID          string
	Model       string
	Tokens      int64
	WindowStart time.Time
}

type chargeKey struct{}

// WithCharge returns a context carrying the admission-time token charge.
func WithCharge(ctx context.Context, c Charge) context.Context {
	return context.WithValue(ctx, chargeKey{}, c)
}

// ChargeFromContext returns the admission-time token charge, if any.
func ChargeFromContext(ctx context.Context) (Charge, bool) {
	c, ok := ctx.Value(chargeKey{}).(Charge)
	return c, ok
}
