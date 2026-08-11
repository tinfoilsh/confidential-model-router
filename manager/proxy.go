package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const (
	// responseHeaderTimeout is the maximum time to wait for the backend to
	// start responding (send response headers). This catches hanging backends
	// without affecting long-running streaming responses, since the timeout
	// only applies before the first byte is received.
	responseHeaderTimeout = 60 * time.Second

	// UsageMetricsRequestHeader is the request header clients set to request usage metrics
	UsageMetricsRequestHeader = "X-Tinfoil-Request-Usage-Metrics"
	// UsageMetricsResponseHeader is the response header (or trailer) containing usage metrics
	UsageMetricsResponseHeader = "X-Tinfoil-Usage-Metrics"
	// maxUsageMetricsBodyBytes caps buffering for non-streaming usage extraction.
	maxUsageMetricsBodyBytes = int64(10 << 20)
	// websearchModel is charged per-request in addition to per-token.
	websearchModel = "websearch"
)

type billingEventCollector interface {
	AddEvent(billing.Event)
	Enabled() bool
}

type routerBillingStateKey struct{}

type routerBillingState struct {
	owned atomic.Bool
	once  sync.Once
}

func billingStateFromRequest(req *http.Request) *routerBillingState {
	state, _ := req.Context().Value(routerBillingStateKey{}).(*routerBillingState)
	return state
}

func removeConnectionToken(header http.Header, token string) {
	var kept []string
	for _, value := range header.Values("Connection") {
		for _, candidate := range strings.Split(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate != "" && !strings.EqualFold(candidate, token) {
				kept = append(kept, candidate)
			}
		}
	}
	if len(kept) == 0 {
		header.Del("Connection")
		return
	}
	header.Set("Connection", strings.Join(kept, ", "))
}

func hasMediaType(header, want string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	return err == nil && strings.EqualFold(mediaType, want)
}

// tokenLabelsKey carries the landing pool and priority class from the
// dispatch site to the proxy's usage handler — the only place per-request
// token counts are known.
type tokenLabelsKey struct{}

type tokenLabels struct{ pool, priority string }

// WithTokenMetricLabels attaches the labels used on the per-request token
// histograms. Requests dispatched without them (tool-loop internals,
// websocket sessions) are not observed.
func WithTokenMetricLabels(ctx context.Context, pool, priority string) context.Context {
	return context.WithValue(ctx, tokenLabelsKey{}, tokenLabels{pool: pool, priority: priority})
}

func observeTokenUsage(ctx context.Context, model string, streaming bool, usage *tokencount.Usage) {
	labels, ok := ctx.Value(tokenLabelsKey{}).(tokenLabels)
	if !ok {
		return
	}
	stream := "non_streaming"
	if streaming {
		stream = "streaming"
	}
	RequestPromptTokens.WithLabelValues(model, labels.pool, labels.priority, stream).
		Observe(float64(usage.PromptTokens))
	RequestCompletionTokens.WithLabelValues(model, labels.pool, labels.priority, stream).
		Observe(float64(usage.CompletionTokens))
	cached, _ := usage.CachedPromptTokens()
	RequestCachedPromptTokensTotal.WithLabelValues(model, labels.pool, labels.priority, stream).
		Add(float64(cached))
}

// classifyProxyError maps a transport-level error to a bounded set of reason
// labels for Prometheus. The raw error message is logged but not exposed as a
// label to avoid unbounded cardinality.
func classifyProxyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, tinfoilClient.ErrCertMismatch) {
		return "tls_mismatch"
	}
	if errors.Is(err, tinfoilClient.ErrNoTLS) || errors.Is(err, tinfoilClient.ErrNoValidCertificate) {
		return "tls_error"
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if errors.Is(netErr.Err, syscall.ECONNREFUSED) {
			return "connection_refused"
		}
		if errors.Is(netErr.Err, syscall.ECONNRESET) {
			return "connection_reset"
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	return "transport_error"
}

func httpFailureReason(statusCode int) string {
	return fmt.Sprintf("http_%d", statusCode)
}

// OpenAI-compatible error type strings returned in API error responses.
const (
	ErrTypeInvalidRequest    = "invalid_request_error"
	ErrTypeInsufficientQuota = "insufficient_quota"
	ErrTypeServer            = "server_error"
)

// Client-facing error messages, aligned with OpenAI's standard error messages
// where applicable. See https://platform.openai.com/docs/guides/error-codes
const (
	ErrMsgServerError   = "The server had an error while processing your request."
	ErrMsgOverloaded    = "The engine is currently overloaded, please try again later."
	ErrMsgModelNotFound = "The model does not exist."
	ErrMsgBodyTooLarge  = "Request body is too large."
)

// billingCloser wraps a response body and emits a zero-token billing event
// on Close() if the usageHandler was never called. This ensures per-request
// models (e.g. docling, whisper) that don't return usage fields still
// generate billing events.
type billingCloser struct {
	io.ReadCloser
	handlerCalled *atomic.Bool
	emitEvent     func()
	once          sync.Once
}

func (b *billingCloser) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		if !b.handlerCalled.Load() {
			b.emitEvent()
		}
	})
	return err
}

// slowHeaderTripper wraps an http.RoundTripper and passively detects when a
// backend takes longer than the configured timeout to send response headers.
// Unlike a hard timeout, the request is never killed — the onSlow callback is
// invoked for circuit breaker / metrics bookkeeping while the request continues.
type slowHeaderTripper struct {
	base    http.RoundTripper
	timeout time.Duration
	onSlow  func()
}

func (t *slowHeaderTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	timer := time.AfterFunc(t.timeout, t.onSlow)
	resp, err := t.base.RoundTrip(req)
	timer.Stop()
	return resp, err
}

// publishBreakerState republishes the breaker gauge after a request
// outcome — unless the enclave was retired: its series was deleted at
// shutdown and a draining request must not resurrect it.
func publishBreakerState(modelName, host string, cb *circuitBreaker) {
	cb.publishIfActive(func(state cbState) {
		CircuitBreakerState.WithLabelValues(modelName, host).Set(float64(state))
	})
}

func newProxy(host, publicKeyFP, modelName string, billingCollector billingEventCollector, cb *circuitBreaker, usageContextSecret string) *httputil.ReverseProxy {
	collectorEnabled := billingCollector != nil && billingCollector.Enabled()
	emitBillingEvent := func(req *http.Request, event billing.Event) {
		state := billingStateFromRequest(req)
		if !collectorEnabled || state == nil || !state.owned.Load() || event.APIKey == "" {
			return
		}
		state.once.Do(func() { billingCollector.AddEvent(event) })
	}
	recordFailure := func(reason string) {
		ProxyFailureTotal.WithLabelValues(modelName, host, reason).Inc()
		cb.RecordFailure()
		publishBreakerState(modelName, host, cb)
	}
	recordSuccess := func() {
		cb.RecordSuccess()
		ProxySuccessTotal.WithLabelValues(modelName, host).Inc()
		publishBreakerState(modelName, host, cb)
	}

	transport := &slowHeaderTripper{
		base: &tinfoilClient.TLSBoundRoundTripper{
			ExpectedPublicKey: publicKeyFP,
		},
		timeout: responseHeaderTimeout,
		onSlow: func() {
			log.WithFields(log.Fields{
				"model":   modelName,
				"enclave": host,
				"timeout": responseHeaderTimeout,
			}).Warn("backend slow: response headers not received within timeout")
			// Purely observational: the request is still in flight; its
			// terminal outcome will be counted in ProxySuccessTotal or
			// ProxyFailureTotal. Does not feed the breaker.
			SlowHeadersTotal.WithLabelValues(modelName, host).Inc()
		},
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "https",
		Host:   host,
	})
	// Set the outbound Host header to targetso downstream shims see the original host name.
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		req.Host = host
		state := &routerBillingState{}
		*req = *req.WithContext(context.WithValue(req.Context(), routerBillingStateKey{}, state))
		if collectorEnabled {
			req.Header.Set("Accept-Encoding", "identity")
		}
		// These are router-owned headers. Never forward a client-supplied
		// context, including when signing is disabled or fails.
		req.Header.Del(usagereporting.HeaderContext)
		req.Header.Del(usagereporting.HeaderUsageContextSignature)
		// ReverseProxy removes headers named by Connection after Director runs.
		// Remove these nominations before installing the signed context.
		removeConnectionToken(req.Header, usagereporting.HeaderContext)
		removeConnectionToken(req.Header, usagereporting.HeaderUsageContextSignature)
		// Sign a usage-context header declaring that the router has already
		// counted this customer request, so the downstream model enclave
		// (shim) suppresses its own billing event and the request is not
		// double-billed on the cloud path (router → shim). Direct-path
		// clients hold no signing secret and cannot forge this header, so
		// they are still billed normally. Fail open for inference: on error
		// the request is forwarded without the header and the shim bills.
		apiKey := BearerToken(req.Header.Get("Authorization"))
		if collectorEnabled && usageContextSecret != "" && apiKey != "" {
			if err := usagereporting.SetHeaders(req.Header, usagereporting.Context{
				ParentService:       usagereporting.ServiceRouter,
				APIKeyHash:          usagereporting.HashAPIKey(apiKey),
				BillCustomerRequest: false,
				Depth:               1,
				IssuedAt:            time.Now().UTC(),
			}, usageContextSecret); err != nil {
				req.Header.Del(usagereporting.HeaderContext)
				req.Header.Del(usagereporting.HeaderUsageContextSignature)
				log.WithError(err).Warn("failed to sign usage context for billing suppression; forwarding without header")
			} else {
				state.owned.Store(true)
			}
		}
	}
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// A body-limit trip during the outbound copy (chunked uploads pass
		// the router's Content-Length pre-check) is a client fault: answer
		// 413 and leave the breaker alone, or cheap oversized uploads could
		// take a healthy backend out of rotation. Like a cancellation, a
		// tripped recovery probe must still return the breaker to open.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.WithFields(log.Fields{
				"model":   modelName,
				"enclave": host,
			}).Warn("request body exceeded limit while proxying")
			ProbeClaimFromContext(r.Context()).Abort()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"message": ErrMsgBodyTooLarge,
					"type":    ErrTypeInvalidRequest,
				},
			})
			return
		}

		reason := classifyProxyError(err)
		log.WithFields(log.Fields{
			"model":   modelName,
			"enclave": host,
			"reason":  reason,
			"error":   err.Error(),
		}).Error("proxy error")
		// Client cancellations aren't backend faults — track in a dedicated
		// counter, don't trip the breaker. A cancelled recovery probe must
		// still return the breaker to open, or it strands half-open forever;
		// only the request that owns the claim may release it.
		if reason == "canceled" {
			ClientCancellationsTotal.WithLabelValues(modelName, host).Inc()
			ProbeClaimFromContext(r.Context()).Abort()
		} else {
			recordFailure(reason)
		}
		apiKey := BearerToken(r.Header.Get("Authorization"))
		emitBillingEvent(r, billing.Event{
			Timestamp:   time.Now(),
			UserID:      "authenticated_user",
			APIKey:      apiKey,
			Model:       modelName,
			Enclave:     host,
			RequestPath: r.URL.Path,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": ErrMsgServerError,
				"type":    ErrTypeServer,
			},
		})
	}

	// Add token extraction and billing via ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= 500 {
			recordFailure(httpFailureReason(resp.StatusCode))
		} else {
			recordSuccess()
		}

		// Extract request details that we'll need for billing
		req := resp.Request
		authHeader := req.Header.Get("Authorization")
		userID := ""
		apiKey := ""
		if authHeader != "" {
			// Extract API key from "Bearer <api_key>" format
			if apiKey = BearerToken(authHeader); apiKey != "" {
				// For user ID, we can use a placeholder or the API key itself
				userID = "authenticated_user"
			}
		}

		requestID := resp.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = resp.Header.Get("X-Request-ID")
		}

		requestPath := req.URL.Path
		streaming := hasMediaType(resp.Header.Get("Content-Type"), "text/event-stream")

		emitZeroTokenEvent := func() {
			emitBillingEvent(req, billing.Event{
				Timestamp:   time.Now(),
				UserID:      userID,
				APIKey:      apiKey,
				Model:       modelName,
				RequestID:   requestID,
				Enclave:     host,
				RequestPath: requestPath,
				Streaming:   streaming,
			})
		}

		// WebSocket/protocol upgrade: the reverse proxy will hijack both
		// connections and copy bidirectionally after this callback returns.
		// We must not touch the body or wrap it, but still emit a billing event.
		if resp.StatusCode == http.StatusSwitchingProtocols {
			emitZeroTokenEvent()
			return nil
		}
		if resp.StatusCode >= http.StatusInternalServerError {
			emitZeroTokenEvent()
			return nil
		}

		// Check if client requested usage metrics in response header/trailer
		usageMetricsRequested := req.Header.Get(UsageMetricsRequestHeader) == "true"
		if streaming && usageMetricsRequested {
			addTrailerHeader(resp.Header, UsageMetricsResponseHeader)
			if wrapper, ok := req.Context().Value(usageWriterKey{}).(*usageMetricsWriter); ok {
				wrapper.EnableTrailer()
			}
		}

		var handlerCalled atomic.Bool

		// Create a usage handler that will be called when usage is extracted
		usageHandler := func(usage *tokencount.Usage) {
			handlerCalled.Store(true)
			if usage == nil {
				return
			}

			// For streaming responses, set usage on wrapper for trailer
			// (non-streaming sets header directly in the buffering block below)
			if streaming && usageMetricsRequested {
				if wrapper, ok := req.Context().Value(usageWriterKey{}).(*usageMetricsWriter); ok {
					wrapper.SetUsage(usage)
				}
			}

			// The websearch parent call is skipped like its billing event:
			// the inner responder call re-enters the proxy and records its own.
			if modelName != websearchModel {
				observeTokenUsage(req.Context(), modelName, streaming, usage)
			}

			// Add billing event
			if collectorEnabled {
				if modelName == websearchModel {
					// Only emit the per-request websearch fee. Skip the
					// token-based event because the websearch service's
					// responder call goes through the proxy with the
					// user's API key and gets billed there directly.
					emitZeroTokenEvent()
				} else {
					cachedPromptTokens := 0
					if value, ok := usage.CachedPromptTokens(); ok {
						cachedPromptTokens = value
					}
					event := billing.Event{
						Timestamp:          time.Now(),
						UserID:             userID,
						APIKey:             apiKey,
						Model:              modelName,
						PromptTokens:       usage.PromptTokens,
						CachedPromptTokens: cachedPromptTokens,
						CompletionTokens:   usage.CompletionTokens,
						TotalTokens:        usage.TotalTokens,
						RequestID:          requestID,
						Enclave:            host,
						RequestPath:        requestPath,
						Streaming:          streaming,
					}
					emitBillingEvent(req, event)
				}
			}
		}

		// Check if client requested usage stats (from header set in main.go)
		clientRequestedUsage := req.Header.Get("X-Tinfoil-Client-Requested-Usage") == "true"

		// For non-streaming JSON responses with usage metrics requested,
		// buffer the response to extract usage and set header before sending
		if !streaming && usageMetricsRequested && resp.StatusCode == http.StatusOK &&
			strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			// Buffer the entire response (bounded).
			limited := io.LimitReader(resp.Body, maxUsageMetricsBodyBytes+1)
			bodyBytes, err := io.ReadAll(limited)
			if err != nil {
				log.WithError(err).Error("Failed to read response body for usage extraction")
				return err
			}
			if int64(len(bodyBytes)) > maxUsageMetricsBodyBytes {
				log.WithField("max_bytes", maxUsageMetricsBodyBytes).
					Warn("Usage metrics extraction skipped: response body exceeds limit")
				resp.Body = withPrefixedBody(bodyBytes, resp.Body)
				emitZeroTokenEvent()
				return nil
			}
			resp.Body.Close()

			// Extract usage from JSON
			var jsonResp struct {
				Usage *tokencount.Usage `json:"usage"`
			}
			if err := json.Unmarshal(bodyBytes, &jsonResp); err == nil && jsonResp.Usage != nil {
				jsonResp.Usage.Normalize()
				// Call usage handler for billing
				usageHandler(jsonResp.Usage)

				// Set usage header directly on response
				resp.Header.Set(UsageMetricsResponseHeader, FormatUsage(jsonResp.Usage, modelName))
			} else if collectorEnabled && apiKey != "" {
				emitZeroTokenEvent()
			}

			// Update Content-Length and restore body
			resp.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			return nil
		}

		// For streaming responses, use the standard token extraction with handler
		// (usage will be set as trailer via the wrapper)
		newBody, err := tokencount.ExtractTokensFromResponseWithHandler(resp, usageHandler, clientRequestedUsage)
		if err != nil {
			log.WithError(err).Error("Failed to extract tokens from response")
			// Don't fail the request, just log the error
			return nil
		}

		// Replace the response body with our new reader
		resp.Body = newBody

		// For non-streaming successful responses, wrap the body so a billing
		// event is emitted on Close() even when the response has no usage field
		// (e.g. docling, whisper). The billingCloser only fires if the
		// usageHandler was never called, preventing double-billing for models
		// that do include usage.
		if !streaming && collectorEnabled && apiKey != "" && resp.StatusCode == http.StatusOK {
			resp.Body = &billingCloser{
				ReadCloser:    resp.Body,
				handlerCalled: &handlerCalled,
				emitEvent:     emitZeroTokenEvent,
			}
		}

		if streaming {
			resp.Header.Del("Content-Length")
		}

		return nil
	}

	return proxy
}

// FormatUsage formats token usage for the response header. It is the single
// source of truth for the header format so every path that emits usage
// metrics produces an identical value.
func FormatUsage(usage *tokencount.Usage, model string) string {
	formatted := "prompt=" + strconv.Itoa(usage.PromptTokens) +
		",completion=" + strconv.Itoa(usage.CompletionTokens) +
		",total=" + strconv.Itoa(usage.TotalTokens)

	if cachedPromptTokens, ok := usage.CachedPromptTokens(); ok {
		uncachedPromptTokens := max(0, usage.PromptTokens-cachedPromptTokens)
		formatted += ",cached_prompt_tokens=" + strconv.Itoa(cachedPromptTokens) +
			",uncached_prompt_tokens=" + strconv.Itoa(uncachedPromptTokens)
	}

	if model != "" {
		formatted += ",model=" + model
	}

	return formatted
}

func addTrailerHeader(h http.Header, name string) {
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	if canonical == "" {
		return
	}

	existing := h.Values("Trailer")
	seen := make(map[string]bool, len(existing)+1)
	var trailers []string
	for _, value := range existing {
		for _, part := range strings.Split(value, ",") {
			trailer := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(part))
			if trailer == "" {
				continue
			}
			if !seen[trailer] {
				seen[trailer] = true
				trailers = append(trailers, trailer)
			}
		}
	}

	if !seen[canonical] {
		trailers = append(trailers, canonical)
	}

	if len(trailers) == 0 {
		return
	}

	h.Set("Trailer", strings.Join(trailers, ", "))
}

type prefixedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (p *prefixedReadCloser) Close() error {
	return p.closer.Close()
}

func withPrefixedBody(prefix []byte, body io.ReadCloser) io.ReadCloser {
	if len(prefix) == 0 {
		return body
	}
	return &prefixedReadCloser{
		Reader: io.MultiReader(bytes.NewReader(prefix), body),
		closer: body,
	}
}
