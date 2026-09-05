package manager

import "context"

// callerPriorityContextKey carries the caller's priority class through
// internal dispatches (the tool loop) so the cache-route reuse metrics
// split them by the originating caller without threading the class through
// every toolruntime signature.
type callerPriorityContextKey struct{}

// WithCallerPriority attaches the caller's priority class ("configured")
// to the request context. The default class carries no information, so an
// empty or "none" value leaves the context untouched.
func WithCallerPriority(ctx context.Context, class string) context.Context {
	if class == "" || class == "none" {
		return ctx
	}
	return context.WithValue(ctx, callerPriorityContextKey{}, class)
}

// CallerPriorityFromContext returns the caller's priority class, or "none"
// when absent.
func CallerPriorityFromContext(ctx context.Context) string {
	if class, _ := ctx.Value(callerPriorityContextKey{}).(string); class != "" {
		return class
	}
	return "none"
}
