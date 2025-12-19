package manager

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
	tinfoilClient "github.com/tinfoilsh/verifier/client"
)

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

		// Create a usage handler that will be called for streaming responses
		usageHandler := func(usage *tokencount.Usage) {
			if usage != nil && billingCollector != nil {
				event := billing.Event{
					Timestamp:        time.Now(),
					UserID:           userID,
					APIKey:           apiKey, // Use the actual API key from Authorization header
					Model:            modelName,
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
		apiType := tokencount.APIType(req.Header.Get("X-Tinfoil-API-Type"))

		// Extract tokens with the usage handler
		newBody, _, err := tokencount.ExtractTokensFromResponseWithHandler(resp, modelName, usageHandler, clientRequestedUsage, apiType)
		if err != nil {
			log.WithError(err).Error("Failed to extract tokens from response")
			// Don't fail the request, just log the error
			return nil
		}

		// Replace the response body with our new reader
		resp.Body = newBody

		if streaming {
			resp.Header.Del("Content-Length")
		}

		return nil
	}

	return proxy
}
