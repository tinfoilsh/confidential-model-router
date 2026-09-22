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

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

const (
	routeContextPath          = "/api/shim/route-context"
	routeContextLookupTimeout = 500 * time.Millisecond
	routeContextResponseLimit = 64 << 10
	demotedPriority           = 1
	decisionAllowed           = "allowed"
	decisionDemote            = "demote"
	decisionRejected          = "rejected"
	decisionExempt            = "exempt"
	rateReasonRequests        = "requests"
	rateReasonTokens          = "tokens"
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

// valid enforces the decision contract documented in the control plane's
// docs/model-rate-limits.md. Anything else is treated as a lookup outage.
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

// routeContextUnavailable records a lookup the control plane did not answer.
// The request is still served: authentication happens again at the enclave,
// so the only thing lost is this one request's shared-quota accounting and
// any configured priority, and a control plane outage must not become an
// inference outage. The counter and log line make the degradation visible.
func routeContextUnavailable(model, reason string) routeContext {
	manager.RouteContextLookupFailuresTotal.WithLabelValues(model, reason).Inc()
	log.WithFields(log.Fields{"model": model, "reason": reason}).Debug("route-context lookup unavailable, admitting without quota decision")
	return routeContext{}
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
// A missing credential and every verdict the control plane returns are
// enforced; a lookup the control plane cannot answer admits the request with
// an empty context.
func (c *routeContextClient) Lookup(ctx context.Context, apiKey, model string) (routeContext, *routeContextError) {
	if apiKey == "" {
		return routeContext{}, &routeContextError{apiError: &manager.ErrMissingAPIKey}
	}
	if model == "" {
		return routeContext{}, &routeContextError{apiError: manager.ErrInvalidRequest.WithParam("model").WithMessage(manager.ErrMsgMissingParam, "model")}
	}
	resolved, err := c.fetch(ctx, apiKey, model)
	if err != nil {
		return routeContext{}, err
	}
	if resolved.RateLimit == nil {
		// JWT bearers skip the lookup and carry no decision.
		return resolved, nil
	}
	return resolved, applyRateDecision(model, resolved.RateLimit)
}

// Metadata deliberately omits model: token counting must never consume an
// inference admission, including when metadata resolution fails.
func (c *routeContextClient) Metadata(ctx context.Context, apiKey string) (routeContext, *routeContextError) {
	return c.fetch(ctx, apiKey, "")
}

// fetch performs one uncached, non-replayable call and validates the response
// shape. It does not act on the decision; Lookup does. Only credential
// denials are returned as errors; anything that prevents a decision from
// being read yields an empty context.
func (c *routeContextClient) fetch(ctx context.Context, apiKey, model string) (routeContext, *routeContextError) {
	if jwtSubject(apiKey) != "" {
		return routeContext{}, nil
	}
	if apiKey == "" {
		return routeContext{}, &routeContextError{apiError: &manager.ErrMissingAPIKey}
	}
	if c == nil || c.endpoint == "" || c.httpClient == nil {
		return routeContextUnavailable(model, "configuration"), nil
	}
	body, err := json.Marshal(routeContextRequest{APIKey: apiKey, Model: model})
	if err != nil {
		return routeContextUnavailable(model, "marshal"), nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, routeContextLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return routeContextUnavailable(model, "request"), nil
	}
	// Admission is not replayable, even after a connection failure or redirect.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return routeContextUnavailable(model, "transport"), nil
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, routeContextResponseLimit+1))
	if readErr != nil || len(data) > routeContextResponseLimit {
		data = nil
	}
	if denial := credentialDenial(resp, data); denial != nil {
		return routeContext{}, denial
	}
	if resp.StatusCode != http.StatusOK {
		return routeContextUnavailable(model, fmt.Sprintf("http_%d", resp.StatusCode)), nil
	}
	var resolved routeContext
	if data == nil || json.Unmarshal(data, &resolved) != nil {
		return routeContextUnavailable(model, "decode"), nil
	}
	if model != "" && !resolved.RateLimit.valid() {
		return routeContextUnavailable(model, "decision"), nil
	}
	return resolved, nil
}

// credentialDenial translates the control plane's credential, payment, and
// per-key quota verdicts. These are answers about the caller, not outages, so
// they keep their status. Only recognized OpenAI-shaped bodies are forwarded;
// the control plane's private error text is otherwise replaced with the
// status text. Per-key quota exhaustion carries Retry-After only when the
// control plane sent a well-formed one, since a lifetime cap has no reset.
func credentialDenial(resp *http.Response, data []byte) *routeContextError {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired, http.StatusTooManyRequests:
	default:
		return nil
	}
	quota := resp.StatusCode == http.StatusTooManyRequests
	apiErr := manager.ErrInvalidRequest.WithStatus(resp.StatusCode).WithMessage("%s", http.StatusText(resp.StatusCode))
	if quota {
		apiErr = manager.ErrRateLimited.WithMessage("%s", http.StatusText(resp.StatusCode))
	}
	if data != nil {
		if normalized, ok := manager.NormalizeUpstreamError(resp.StatusCode, data); ok {
			apiErr = normalized
		} else if !quota {
			var payload struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
				apiErr = apiErr.WithMessage("%s", payload.Error)
			}
		}
	}
	denial := &routeContextError{apiError: apiErr}
	if quota {
		denial.retryAfter = quotaRetryAfter(resp.Header)
	}
	return denial
}

// applyRateDecision turns a validated shared-quota rejection into the client
// response. Demote and exempt are applied later, when the body is rewritten.
func applyRateDecision(model string, decision *routeRateLimit) *routeContextError {
	if decision.Decision != decisionRejected {
		return nil
	}
	retry := *decision.RetryAfterSeconds
	message := manager.ErrMsgRateLimited
	if decision.Reason == rateReasonTokens {
		message = manager.ErrMsgTokenRateLimited
	}
	manager.RateLimitRejectionsTotal.WithLabelValues(model).Inc()
	manager.RateLimitRejectionsByReasonTotal.WithLabelValues(model, decision.Reason).Inc()
	return &routeContextError{
		apiError:   manager.ErrRateLimited.WithMessage(message, retry),
		retryAfter: strconv.FormatInt(retry, 10),
	}
}

func (r routeContext) overloadExempt() bool {
	return r.Priority != nil || (r.RateLimit != nil && r.RateLimit.Decision == decisionExempt)
}

// applyPriority owns the body's vLLM priority field on endpoints whose schema
// accepts it: client-supplied values are always stripped, then a demotion or a
// configured organization priority is injected.
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
