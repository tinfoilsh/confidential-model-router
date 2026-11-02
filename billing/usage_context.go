package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const (
	// harnessService is the only parent whose usage context may suppress a
	// customer request. The chat harness calls the router directly and counts
	// each run once, on its first model turn.
	harnessService      = "harness"
	harnessCallDepth    = 1
	usageContextMaxSkew = 10 * time.Minute
)

type customerRequestsKey struct{}

// CustomerRequestCount returns how many customer requests a response to a
// request with header h bills. A request without a usage context bills one.
// A usage context that is present but fails verification is an error, never
// a fallthrough to billing.
func CustomerRequestCount(h http.Header, apiKey, secret string) (int64, error) {
	usage, signed, err := usagereporting.FromHeaders(h, secret, time.Now(), usageContextMaxSkew)
	if !signed {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	switch {
	case usage.ParentService != harnessService:
		return 0, fmt.Errorf("usage context from parent service %q, which may not suppress billing", usage.ParentService)
	case usage.Depth != harnessCallDepth:
		return 0, fmt.Errorf("usage context at depth %d, which is not a direct call from the harness", usage.Depth)
	case usage.APIKeyHash == "":
		return 0, errors.New("usage context names no API key")
	case !usagereporting.VerifyAPIKeyHash(apiKey, usage.APIKeyHash):
		return 0, errors.New("usage context was issued against a different API key")
	}
	if usage.BillCustomerRequest {
		return 1, nil
	}
	return 0, nil
}

// WithCustomerRequests carries a request's customer-request count to the
// places that build its billing events.
func WithCustomerRequests(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, customerRequestsKey{}, n)
}

// CustomerRequests returns the count WithCustomerRequests attached to ctx, or
// one when there is none.
func CustomerRequests(ctx context.Context) int64 {
	if n, ok := ctx.Value(customerRequestsKey{}).(int64); ok {
		return n
	}
	return 1
}
