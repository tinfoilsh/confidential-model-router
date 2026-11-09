package manager

import "context"

// Caller identifies an authenticated caller for reservation matching.
type Caller struct {
	OrgID  string
	UserID string
}

// callerContextKey carries the caller through internal dispatches so
// reservation pools apply without changing every toolruntime signature.
type callerContextKey struct{}

// WithCaller attaches trusted caller identity to the request context.
func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, caller)
}

// CallerFromContext returns the caller's identity, or an empty identity when absent.
func CallerFromContext(ctx context.Context) Caller {
	caller, _ := ctx.Value(callerContextKey{}).(Caller)
	return caller
}
