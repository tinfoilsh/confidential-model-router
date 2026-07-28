package cacheroute

import "context"

// reuseKey carries a request's reuse classification from the routing
// decision to response-time usage extraction. Only the classification
// travels — never the routing key or any per-key state.
type reuseKey struct{}

type reuseValue struct {
	model string
	reuse string
}

// WithReuse returns a context carrying the request's reuse classification
// for RecordUsage.
func WithReuse(ctx context.Context, model, reuse string) context.Context {
	return context.WithValue(ctx, reuseKey{}, reuseValue{model: model, reuse: reuse})
}

// RecordUsage adds engine-reported token counts to the reuse-classified
// usage counters. A context without a classification (request not keyed,
// or the shadow pipeline didn't run) is a no-op.
func RecordUsage(ctx context.Context, promptTokens, cachedPromptTokens int) {
	v, ok := ctx.Value(reuseKey{}).(reuseValue)
	if !ok {
		return
	}
	UsagePromptTokensTotal.WithLabelValues(v.model, v.reuse).Add(float64(promptTokens))
	UsageCachedPromptTokensTotal.WithLabelValues(v.model, v.reuse).Add(float64(cachedPromptTokens))
}
