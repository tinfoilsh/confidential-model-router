package manager

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
	tinfoilClient "github.com/tinfoilsh/verifier/client"
)

const (
	// UsageMetricsRequestHeader is the request header clients set to request usage metrics
	UsageMetricsRequestHeader = "X-Tinfoil-Request-Usage-Metrics"
	// UsageMetricsResponseHeader is the response header (or trailer) containing usage metrics
	UsageMetricsResponseHeader = "X-Tinfoil-Usage-Metrics"
	// maxUsageMetricsBodyBytes caps buffering for non-streaming usage extraction.
	maxUsageMetricsBodyBytes = int64(10 << 20)
	// websearchModel is charged per-request in addition to per-token.
	websearchModel = "websearch"
)

// OpenAI-compatible error type strings returned in API error responses.
const (
	ErrTypeInvalidRequest    = "invalid_request_error"
	ErrTypeInsufficientQuota = "insufficient_quota"
	ErrTypeServer            = "server_error"
)

// RequestModelKey stores the model name extracted from the request body,
// used to bill token usage under the underlying model for tool requests.
type RequestModelKey struct{}

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

func newProxy(host, publicKeyFP, modelName string, billingCollector *billing.Collector) *httputil.ReverseProxy {
	httpClient := &http.Client{
		Transport: &tinfoilClient.TLSBoundRoundTripper{
			ExpectedPublicKey: publicKeyFP,
		},
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "https",
		Host:   host,
	})
	proxy.Transport = httpClient.Transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Errorf("proxy error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "upstream enclave unavailable",
				"type":    ErrTypeServer,
			},
		})
	}

	// Add token extraction and billing via ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Extract request details that we'll need for billing
		req := resp.Request
		authHeader := req.Header.Get("Authorization")
		userID := ""
		apiKey := ""
		if authHeader != "" {
			// Extract API key from "Bearer <api_key>" format
			if strings.HasPrefix(authHeader, "Bearer ") {
				apiKey = strings.TrimPrefix(authHeader, "Bearer ")
				// For user ID, we can use a placeholder or the API key itself
				userID = "authenticated_user"
			}
		}

		requestID := resp.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = resp.Header.Get("X-Request-ID")
		}

		requestPath := req.URL.Path
		streaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

		// Check if client requested usage metrics in response header/trailer
		usageMetricsRequested := req.Header.Get(UsageMetricsRequestHeader) == "true"
		if streaming && usageMetricsRequested {
			addTrailerHeader(resp.Header, UsageMetricsResponseHeader)
			if wrapper, ok := req.Context().Value(usageWriterKey{}).(*usageMetricsWriter); ok {
				wrapper.EnableTrailer()
			}
		}

		var handlerCalled atomic.Bool

		emitZeroTokenEvent := func() {
			billingCollector.AddEvent(billing.Event{
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

			// Add billing event
			if billingCollector != nil {
				if modelName == websearchModel {
					emitZeroTokenEvent()
				}
				// Use the underlying model for token billing when set (e.g., websearch
				// bills tokens under the model specified in the inference request).
				billingModel := modelName
				if bm, ok := req.Context().Value(RequestModelKey{}).(string); ok && bm != "" {
					billingModel = bm
				}
				event := billing.Event{
					Timestamp:        time.Now(),
					UserID:           userID,
					APIKey:           apiKey,
					Model:            billingModel,
					PromptTokens:     usage.PromptTokens,
					CompletionTokens: usage.CompletionTokens,
					TotalTokens:      usage.TotalTokens,
					RequestID:        requestID,
					Enclave:          host,
					RequestPath:      requestPath,
					Streaming:        streaming,
				}
				billingCollector.AddEvent(event)
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
				return nil
			}
			resp.Body.Close()

			// Extract usage from JSON
			var jsonResp struct {
				Usage *tokencount.Usage `json:"usage"`
			}
			if err := json.Unmarshal(bodyBytes, &jsonResp); err == nil && jsonResp.Usage != nil {
				// Call usage handler for billing
				usageHandler(jsonResp.Usage)

				// Set usage header directly on response
				resp.Header.Set(UsageMetricsResponseHeader, formatUsage(jsonResp.Usage))
			} else if billingCollector != nil && apiKey != "" {
				emitZeroTokenEvent()
			}

			// Update Content-Length and restore body
			resp.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			return nil
		}

		// For streaming responses, use the standard token extraction with handler
		// (usage will be set as trailer via the wrapper)
		newBody, _, err := tokencount.ExtractTokensFromResponseWithHandler(resp, modelName, usageHandler, clientRequestedUsage)
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
		if !streaming && billingCollector != nil && apiKey != "" && resp.StatusCode == http.StatusOK {
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

// formatUsage formats token usage for the response header
func formatUsage(usage *tokencount.Usage) string {
	return "prompt=" + strconv.Itoa(usage.PromptTokens) +
		",completion=" + strconv.Itoa(usage.CompletionTokens) +
		",total=" + strconv.Itoa(usage.TotalTokens)
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
