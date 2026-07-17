package manager

import "context"

// callerOrgContextKey carries the caller's org through internal dispatches
// (the tool loop) so reservation pools apply to them without threading the
// org through every toolruntime signature.
type callerOrgContextKey struct{}

// WithCallerOrg attaches the caller's org id to the request context.
func WithCallerOrg(ctx context.Context, orgID string) context.Context {
	if orgID == "" {
		return ctx
	}
	return context.WithValue(ctx, callerOrgContextKey{}, orgID)
}

// CallerOrgFromContext returns the caller's org id, or "" when absent.
func CallerOrgFromContext(ctx context.Context) string {
	orgID, _ := ctx.Value(callerOrgContextKey{}).(string)
	return orgID
}
