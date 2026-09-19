package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

const (
	routeContextPath              = "/api/shim/route-context"
	routeContextLookupTimeout     = 500 * time.Millisecond
	routeContextRetryAfterSeconds = 1
	routeContextResponseLimit     = 64 << 10
	demotedPriority               = 1
	decisionAllowed               = "allowed"
	decisionDemote                = "demote"
	decisionRejected              = "rejected"
	decisionExempt                = "exempt"
	rateReasonRequests            = "requests"
	rateReasonTokens              = "tokens"
)

type routeContextClient struct {
	endpoint   string
	httpClient *http.Client
}

type routeContextRequest struct {
	APIKey string `json:"api_key"`
	Model  string `json:"model,omitempty"`
}

type routeContext struct {
	Priority  *int            `json:"priority,omitempty"`
	OrgID     string          `json:"org_id,omitempty"`
	RateLimit *routeRateLimit `json:"rate_limit,omitempty"`
}

type routeRateLimit struct {
	Decision          string      `json:"decision"`
	Reason            string      `json:"reason,omitempty"`
	RetryAfterSeconds *int64      `json:"retry_after_seconds"`
	Requests          *routeQuota `json:"requests,omitempty"`
	Tokens            *routeQuota `json:"tokens,omitempty"`
}

type routeQuota struct {
	Limit *int64 `json:"limit"`
	Used  *int64 `json:"used"`
}

func (q *routeQuota) valid() bool {
	return q == nil || (q.Limit != nil && q.Used != nil && *q.Limit >= 0 && *q.Used >= 0)
}

func (r *routeRateLimit) valid() bool {
	if r == nil || r.RetryAfterSeconds == nil || *r.RetryAfterSeconds < 0 || !r.Requests.valid() || !r.Tokens.valid() {
		return false
	}
	if r.Reason != "" && r.Reason != rateReasonRequests && r.Reason != rateReasonTokens {
		return false
	}
	switch r.Decision {
	case decisionAllowed, decisionExempt:
		return r.Reason == ""
	case decisionDemote:
		return r.Reason == rateReasonRequests
	case decisionRejected:
		return r.Reason != "" && *r.RetryAfterSeconds > 0
	default:
		return false
	}
}

type routeContextError struct {
	apiError   *manager.APIError
	retryAfter string
}

func (e *routeContextError) Error() string { return e.apiError.Error() }

func (e *routeContextError) write(w http.ResponseWriter) {
	if e.retryAfter != "" {
		w.Header().Set("Retry-After", e.retryAfter)
	}
	writeError(w, e.apiError)
}

func quotaRetryAfter(header http.Header) string {
	values := header.Values("Retry-After")
	if len(values) != 1 {
		return ""
	}
	value := strings.TrimSpace(values[0])
	if _, err := strconv.ParseUint(value, 10, 64); err == nil {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

func routeContextUnavailable(model, reason string) *routeContextError {
	manager.RouteContextLookupFailuresTotal.WithLabelValues(model, reason).Inc()
	return &routeContextError{
		apiError:   &manager.ErrAdmissionUnavailable,
		retryAfter: strconv.Itoa(routeContextRetryAfterSeconds),
	}
}

func newRouteContextClient(controlPlaneURL string) *routeContextClient {
	return &routeContextClient{
		endpoint:   strings.TrimRight(controlPlaneURL, "/") + routeContextPath,
		httpClient: &http.Client{},
	}
}

// Lookup applies quota admission to one external inference request. JWT shape
// classification skips only this quota lookup, not authentication: downstream
// model and tool shims must verify the credential before serving inference.
func (c *routeContextClient) Lookup(ctx context.Context, apiKey, model string) (routeContext, *routeContextError) {
	if model == "" {
		return routeContext{}, routeContextUnavailable(model, "missing_model")
	}
	return c.fetch(ctx, apiKey, model)
}

// Metadata deliberately omits model: token counting must never consume an
// inference admission, including when metadata resolution fails.
func (c *routeContextClient) Metadata(ctx context.Context, apiKey string) (routeContext, *routeContextError) {
	return c.fetch(ctx, apiKey, "")
}

func (c *routeContextClient) fetch(ctx context.Context, apiKey, model string) (routeContext, *routeContextError) {
	if jwtSubject(apiKey) != "" {
		return routeContext{}, nil
	}
	if apiKey == "" {
		return routeContext{}, &routeContextError{apiError: &manager.ErrMissingAPIKey}
	}
	if c == nil || c.endpoint == "" || c.httpClient == nil {
		return routeContext{}, routeContextUnavailable(model, "configuration")
	}
	body, err := json.Marshal(routeContextRequest{APIKey: apiKey, Model: model})
	if err != nil {
		return routeContext{}, routeContextUnavailable(model, "marshal")
	}
	reqCtx, cancel := context.WithTimeout(ctx, routeContextLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return routeContext{}, routeContextUnavailable(model, "request")
	}
	// Admission is not replayable, even after a connection failure or redirect.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return routeContext{}, routeContextUnavailable(model, "transport")
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, routeContextResponseLimit+1))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
		apiErr := manager.ErrInvalidRequest.WithStatus(resp.StatusCode).WithMessage("%s", http.StatusText(resp.StatusCode))
		if resp.StatusCode == http.StatusTooManyRequests {
			apiErr = manager.ErrRateLimited.WithMessage("%s", http.StatusText(resp.StatusCode))
		}
		if readErr == nil && len(data) <= routeContextResponseLimit {
			if normalized, ok := manager.NormalizeUpstreamError(resp.StatusCode, data); ok {
				apiErr = normalized
			} else if resp.StatusCode != http.StatusTooManyRequests {
				var payload struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
					apiErr = apiErr.WithMessage("%s", payload.Error)
				}
			}
		}
		denial := &routeContextError{apiError: apiErr}
		if resp.StatusCode == http.StatusTooManyRequests {
			denial.retryAfter = quotaRetryAfter(resp.Header)
		}
		return routeContext{}, denial
	}
	if resp.StatusCode != http.StatusOK {
		return routeContext{}, routeContextUnavailable(model, fmt.Sprintf("http_%d", resp.StatusCode))
	}
	var resolved routeContext
	if readErr != nil || len(data) > routeContextResponseLimit || json.Unmarshal(data, &resolved) != nil {
		return routeContext{}, routeContextUnavailable(model, "decode")
	}
	if model != "" && !resolved.RateLimit.valid() {
		return routeContext{}, routeContextUnavailable(model, "decision")
	}
	if model != "" && resolved.RateLimit.Decision == decisionRejected {
		retry := *resolved.RateLimit.RetryAfterSeconds
		message := manager.ErrMsgRateLimited
		if resolved.RateLimit.Reason == rateReasonTokens {
			message = manager.ErrMsgTokenRateLimited
		}
		manager.RateLimitRejectionsTotal.WithLabelValues(model).Inc()
		manager.RateLimitRejectionsByReasonTotal.WithLabelValues(model, resolved.RateLimit.Reason).Inc()
		return routeContext{}, &routeContextError{
			apiError:   manager.ErrRateLimited.WithMessage(message, retry),
			retryAfter: strconv.FormatInt(retry, 10),
		}
	}
	return resolved, nil
}

func (r routeContext) overloadExempt() bool {
	return r.Priority != nil || (r.RateLimit != nil && r.RateLimit.Decision == decisionExempt)
}

func (r routeContext) applyPriority(body map[string]any, path, model string) {
	delete(body, "priority")
	if !cacheSaltPaths[path] || body == nil {
		return
	}
	if r.RateLimit != nil && r.RateLimit.Decision == decisionDemote {
		body["priority"] = demotedPriority
		manager.RateLimitDemotionsTotal.WithLabelValues(model).Inc()
	} else if r.Priority != nil {
		body["priority"] = *r.Priority
		manager.PriorityAssignmentsTotal.WithLabelValues(model).Inc()
	}
}
