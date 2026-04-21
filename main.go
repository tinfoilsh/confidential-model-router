package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/toolprofile"
	"github.com/tinfoilsh/confidential-model-router/toolruntime"
)

//go:embed config.yml
var configFile []byte // Initial (attested) config

// Set by build process
var version = "dev"

// getEnvOrDefault returns the environment variable value if set, otherwise returns the default
func getEnvOrDefault(envKey, defaultVal string) string {
	if val := os.Getenv(envKey); val != "" {
		return val
	}
	return defaultVal
}

// getEnvOrDefaultDuration returns the environment variable value parsed as a duration if set, otherwise returns the default
func getEnvOrDefaultDuration(envKey string, defaultVal time.Duration) time.Duration {
	if val := os.Getenv(envKey); val != "" {
		d, err := time.ParseDuration(val)
		if err != nil {
			log.Fatalf("invalid duration for %s: %v", envKey, err)
		}
		return d
	}
	return defaultVal
}

// getEnvBool returns true if the environment variable is set to a truthy value
func getEnvBool(envKey string) bool {
	val := strings.ToLower(os.Getenv(envKey))
	return val == "true" || val == "1" || val == "yes"
}

var (
	port                = flag.String("l", getEnvOrDefault("PORT", "8089"), "port to listen on (env: PORT)")
	controlPlaneURL     = flag.String("C", getEnvOrDefault("CONTROL_PLANE_URL", "https://api.tinfoil.sh"), "control plane URL (env: CONTROL_PLANE_URL)")
	usageReporterID     = flag.String("usage-reporter-id", getEnvOrDefault("USAGE_REPORTER_ID", "model-router"), "usage reporter ID (env: USAGE_REPORTER_ID)")
	usageReporterSecret = flag.String("usage-reporter-secret", getEnvOrDefault("USAGE_REPORTER_SECRET", ""), "usage reporter HMAC secret (env: USAGE_REPORTER_SECRET)")
	verbose             = flag.Bool("v", getEnvBool("VERBOSE"), "enable verbose logging (env: VERBOSE)")
	initConfigURL       = flag.String("i", getEnvOrDefault("INIT_CONFIG_URL", ""), "optional path to initial config.yml (requires to append @sha256:<hex> for integrity) (env: INIT_CONFIG_URL)")
	updateConfigURL     = flag.String("u", getEnvOrDefault("UPDATE_CONFIG_URL", "https://raw.githubusercontent.com/tinfoilsh/confidential-model-router/main/config.yml"), "path to runtime config.yml (env: UPDATE_CONFIG_URL)")
	domain              = flag.String("d", getEnvOrDefault("DOMAIN", "localhost"), "domain used by this router (env: DOMAIN)")
	refreshInterval     = flag.Duration("r", getEnvOrDefaultDuration("REFRESH_INTERVAL", 5*time.Minute), "refresh interval for syncing enclave config (env: REFRESH_INTERVAL)")
	// debug enables non-production behaviors such as honoring
	// LOCAL_MCP_ENDPOINT_<MODEL> env vars to bypass attested TLS
	// pinning for MCP tool servers during local development. MUST
	// NOT be enabled in deployed enclaves.
	debug = flag.Bool("debug", getEnvBool("DEBUG"), "enable debug-only overrides for local development (env: DEBUG)")
)

func jsonError(w http.ResponseWriter, message string, errType string, code int) {
	switch {
	case code >= 500:
		log.Errorf("jsonError: %s", message)
	case code >= 400:
		log.Warnf("jsonError: %s", message)
	default:
		log.Debugf("jsonError: %s", message)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
		},
	})
}

func sendJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			return true
		}
	}
	return false
}

func parseModelFromSubdomain(r *http.Request, domain string) (string, error) {
	// Check if the request is for a subdomain and derive model from leftmost subdomain.
	host := r.Header.Get("X-Forwarded-Host")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	log.Debugf("host (from X-Forwarded-Host): %s", host)
	if !strings.HasSuffix(host, "."+domain) {
		return "", nil
	}

	// If request is for a subdomain, use leftmost label as model name (e.g., deepseek.inference.tinfoil.sh -> deepseek)
	if host != domain && strings.HasSuffix(host, "."+domain) {
		sub := strings.TrimSuffix(host, "."+domain)
		if sub == "" {
			return "", fmt.Errorf("subdomain is empty")
		} else {
			parts := strings.Split(sub, ".")
			if len(parts) > 0 && parts[0] != "" {
				return parts[0], nil
			} else {
				return "", fmt.Errorf("first subdomain is empty")
			}
		}
	}
	return "", nil
}

// extractModelFromMultipart extracts the model name from a multipart form request.
// Returns the model name (empty if not found) and the buffered body bytes for forwarding.
func extractModelFromMultipart(r *http.Request) (string, []byte, error) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read request body: %w", err)
	}
	r.Body.Close()

	contentType := r.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", bodyBytes, nil // Can't parse, return body for forwarding
	}

	boundary := params["boundary"]
	if boundary == "" {
		return "", bodyBytes, nil
	}

	reader := multipart.NewReader(bytes.NewReader(bodyBytes), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", bodyBytes, nil // Parse error, continue with default
		}
		if part.FormName() == "model" {
			modelBytes, _ := io.ReadAll(part)
			part.Close()
			return strings.TrimSpace(string(modelBytes)), bodyBytes, nil
		}
		part.Close()
	}

	return "", bodyBytes, nil
}

// ensureStreamingUsageOptions forces upstream streaming requests to include
// usage and continuous usage stats so billing can extract token counts and all
// models follow the same streaming usage behavior. If the client explicitly
// asked for usage stats, we mark that in a header so the proxy can preserve
// usage-only chunks instead of filtering them out.
func ensureStreamingUsageOptions(body map[string]any, headers http.Header) {
	clientRequestedUsage := false

	streamOptions, ok := body["stream_options"].(map[string]any)
	if !ok {
		streamOptions = map[string]any{}
		body["stream_options"] = streamOptions
	}

	// Check if the client explicitly requested usage stats before we modify the
	// request. The proxy uses this signal to decide whether to filter
	// usage-only chunks from the streamed response.
	if includeUsage, ok := streamOptions["include_usage"].(bool); ok && includeUsage {
		clientRequestedUsage = true
	}
	if continuousUsage, ok := streamOptions["continuous_usage_stats"].(bool); ok && continuousUsage {
		clientRequestedUsage = true
	}

	streamOptions["include_usage"] = true
	streamOptions["continuous_usage_stats"] = true

	if clientRequestedUsage {
		headers.Set("X-Tinfoil-Client-Requested-Usage", "true")
	}
}

func main() {
	flag.Parse()
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	log.Debugf("Configuration: domain=%s, port=%s, controlPlaneURL=%s", *domain, *port, *controlPlaneURL)
	log.Infof("Refresh interval: %s", *refreshInterval)

	em, err := manager.NewEnclaveManager(configFile, *controlPlaneURL, *usageReporterID, *usageReporterSecret, *initConfigURL, *updateConfigURL, *refreshInterval)
	if err != nil {
		log.Fatal(err)
	}
	em.SetDebugMode(*debug)
	if *debug {
		log.Warn("debug mode enabled: local development overrides are active; do not use in production")
	}
	defer em.Shutdown()
	go em.StartWorker()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var modelName string
		var err error

		// Extract API key early for rate limiting decisions
		apiKey := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			apiKey = strings.TrimPrefix(auth, "Bearer ")
		}

		if modelName, err = parseModelFromSubdomain(r, *domain); err != nil {
			jsonError(w, fmt.Sprintf("Invalid request: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
			return
		}

		// WebSocket upgrade on /v1/realtime: extract model from ?model= query parameter, skip body parsing
		if isWebSocketUpgrade(r) && r.URL.Path == "/v1/realtime" {
			if modelName == "" {
				modelName = r.URL.Query().Get("model")
			}
			if modelName == "" {
				jsonError(w, "Missing required parameter: 'model' (use ?model=<name> query parameter for WebSocket requests).", manager.ErrTypeInvalidRequest, http.StatusBadRequest)
				return
			}

			// Browser WebSocket auth: extract API key from Sec-WebSocket-Protocol subprotocol
			// Browsers can't set Authorization headers, so they pass the key as:
			//   new WebSocket(url, ["realtime", "openai-insecure-api-key.<key>"])
			if apiKey == "" {
				const subprotoPrefix = "openai-insecure-api-key."
				var cleaned []string
				for _, proto := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
					proto = strings.TrimSpace(proto)
					if strings.HasPrefix(proto, subprotoPrefix) {
						apiKey = strings.TrimPrefix(proto, subprotoPrefix)
					} else if proto != "" {
						cleaned = append(cleaned, proto)
					}
				}
				if apiKey != "" {
					r.Header.Set("Authorization", "Bearer "+apiKey)
					if len(cleaned) > 0 {
						r.Header.Set("Sec-WebSocket-Protocol", strings.Join(cleaned, ", "))
					} else {
						r.Header.Del("Sec-WebSocket-Protocol")
					}
				}
			}

			log.WithFields(log.Fields{
				"model": modelName,
				"path":  r.URL.Path,
			}).Debug("WebSocket upgrade request")
		} else if modelName == "" { // The request does not use a subdomain. We route using specific inference routing logic.
			if r.URL.Path == "/" {
				http.Redirect(w, r, "https://docs.tinfoil.sh", http.StatusTemporaryRedirect)
				return
			} else if r.URL.Path == "/health" {
				if !em.Ready() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					json.NewEncoder(w).Encode(map[string]any{
						"status": "not ready",
					})
					return
				}
				sendJSON(w, map[string]any{"status": "ok", "version": version})
				return
			} else if r.URL.Path == "/.well-known/tinfoil-proxy" {
				status := em.Status()
				status["version"] = version
				sendJSON(w, status)
				return
			} else if r.URL.Path == "/.well-known/prometheus-targets" {
				// Prometheus HTTP service discovery endpoint
				// See: https://prometheus.io/docs/prometheus/latest/configuration/configuration/#http_sd_config
				sendJSON(w, em.PrometheusTargets())
				return
			} else if r.URL.Path == "/metrics" {
				// Expose Prometheus metrics
				promhttp.Handler().ServeHTTP(w, r)
				return
			} else if r.URL.Path == "/v1/models" {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, *controlPlaneURL+"/v1/models", nil)
				if err != nil {
					jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
					return
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				io.Copy(w, resp.Body)
				return
			} else if r.URL.Path == "/v1/audio/speech" {
				// Extract model from JSON body, default to qwen3-tts
				var body map[string]any
				bodyBytes, err := io.ReadAll(r.Body)
				if err != nil {
					jsonError(w, fmt.Sprintf("Could not read request body: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
					return
				}
				r.Body.Close()
				if err := json.Unmarshal(bodyBytes, &body); err != nil {
					jsonError(w, fmt.Sprintf("Invalid request body: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
					return
				}
				if m, ok := body["model"].(string); ok && m != "" {
					modelName = m
				} else {
					modelName = "qwen3-tts"
				}
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			} else if r.URL.Path == "/v1/audio/transcriptions" || strings.HasPrefix(r.URL.Path, "/v1/audio/") {
				// Extract model from multipart form, default to voxtral-small-24b
				var bodyBytes []byte
				modelName, bodyBytes, err = extractModelFromMultipart(r)
				if err != nil {
					jsonError(w, fmt.Sprintf("Invalid request body: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
					return
				}
				if modelName == "" {
					modelName = "voxtral-small-24b"
				}
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			} else if r.URL.Path == "/v1/convert/file" {
				modelName = "doc-upload"
			} else { // This is an OpenAI-compatible API request
				var body map[string]any
				bodyBytes, err := io.ReadAll(r.Body)

				if err != nil {
					jsonError(w, fmt.Sprintf("Could not read request body: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
					return
				}
				if err := json.Unmarshal(bodyBytes, &body); err != nil {
					jsonError(w, fmt.Sprintf("Invalid request body: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
					return
				}

				// Extract model name from request body
				modelInterface, ok := body["model"]
				if !ok {
					jsonError(w, "Missing required parameter: 'model'.", manager.ErrTypeInvalidRequest, http.StatusBadRequest)
					return
				}
				modelName, ok = modelInterface.(string)
				if !ok {
					jsonError(w, "Invalid parameter: 'model' must be a string.", manager.ErrTypeInvalidRequest, http.StatusBadRequest)
					return
				}

				// Check if request uses web search so the router can run the
				// tool loop locally against the configured tool server profile.
				useWebsearch := false
				if _, ok := body["web_search_options"]; ok {
					useWebsearch = true
				} else if r.URL.Path == "/v1/responses" {
					if tools, ok := body["tools"].([]any); ok {
						useWebsearch = slices.ContainsFunc(tools, func(t any) bool {
							m, _ := t.(map[string]any)
							typeVal, ok := m["type"].(string)
							return ok && typeVal == "web_search"
						})
					}
				}
				rateLimitModel := modelName

				bodyModified := false
				if r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/chat/completions" {
					rewritten := false
					switch r.URL.Path {
					case "/v1/responses":
						rewritten, err = rewriteResponsesBase64Files(r.Context(), body, em, r.Header.Get("Authorization"))
					case "/v1/chat/completions":
						rewritten, err = rewriteChatCompletionsBase64Files(r.Context(), body, em, r.Header.Get("Authorization"))
					}
					if err != nil {
						var inputErr *fileInputError
						if errors.As(err, &inputErr) {
							jsonError(w, inputErr.Message, manager.ErrTypeInvalidRequest, inputErr.StatusCode)
							return
						}

						var conversionErr *manager.FileConversionError
						if errors.As(err, &conversionErr) {
							errType := manager.ErrTypeServer
							if conversionErr.StatusCode >= 400 && conversionErr.StatusCode < 500 {
								errType = manager.ErrTypeInvalidRequest
							}
							jsonError(w, conversionErr.Message, errType, conversionErr.StatusCode)
							return
						}

						jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
						return
					}
					bodyModified = rewritten
				}

				// Strip any user-supplied priority to prevent circumventing rate limits
				// or jumping ahead of other users.
				_, hadPriority := body["priority"]
				delete(body, "priority")

				// Check rate limiting and inject lower vLLM priority if over budget
				bodyModified = bodyModified || hadPriority
				if rlCfg := em.GetRateLimitConfig(rateLimitModel); rlCfg != nil {
					if apiKey != "" && em.RequestTracker().RecordAndCheck(apiKey, rateLimitModel, rlCfg.MaxRequestsPerMinute) {
						body["priority"] = 1
						bodyModified = true
						manager.RateLimitDemotionsTotal.WithLabelValues(rateLimitModel).Inc()
						log.WithFields(log.Fields{
							"model": rateLimitModel,
						}).Debug("rate limited: injecting lower vLLM priority")
					}
				}

				// If streaming request, ensure upstream usage is available for billing.
				if stream, ok := body["stream"].(bool); ok && stream {
					ensureStreamingUsageOptions(body, r.Header)

					// Re-encode the modified body
					newBodyBytes, err := json.Marshal(body)
					if err != nil {
						jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
						return
					}
					bodyBytes = newBodyBytes
					// Update Content-Length header to match new body size
					r.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
					r.ContentLength = int64(len(bodyBytes))
					log.Debugf("Modified streaming request body to include usage for billing, client requested usage: %v",
						r.Header.Get("X-Tinfoil-Client-Requested-Usage") == "true")
				}

				// Re-encode body if rate limiting modified it (non-streaming path)
				if bodyModified {
					newBodyBytes, err := json.Marshal(body)
					if err != nil {
						jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
						return
					}
					bodyBytes = newBodyBytes
					r.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
					r.ContentLength = int64(len(bodyBytes))
				}

				r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

				if useWebsearch {
					if err := toolruntime.Handle(w, r, em, toolprofile.WebSearch, body, modelName); err != nil {
						log.WithError(err).WithFields(log.Fields{
							"model": modelName,
							"path":  r.URL.Path,
						}).Error("tool runtime failed")
						jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
					}
					return
				}
			}
		}

		model, found := em.GetModel(modelName)
		if !found {
			jsonError(w, manager.ErrMsgModelNotFound, manager.ErrTypeInvalidRequest, http.StatusNotFound)
			return
		}

		enclave := model.NextEnclave()
		if enclave == nil {
			jsonError(w, manager.ErrMsgOverloaded, manager.ErrTypeServer, http.StatusServiceUnavailable)
			return
		}

		if overloaded, retryAfter, waiting := enclave.ShouldReject(); overloaded {
			secs := int(retryAfter.Seconds())
			if secs <= 0 {
				secs = 60
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			log.WithFields(log.Fields{
				"model":               modelName,
				"enclave":             enclave.String(),
				"requests_waiting":    waiting,
				"retry_after_seconds": secs,
			}).Warn("rejecting request due to backend overload")

			// Record rejection metrics
			manager.RequestsRejectedTotal.WithLabelValues(modelName).Inc()
			manager.RetryAfterSeconds.WithLabelValues(modelName).Observe(float64(secs))

			jsonError(w, fmt.Sprintf("Request rate exceeded. Retry after %d seconds.", secs), manager.ErrTypeInvalidRequest, http.StatusTooManyRequests)
			return
		}

		log.Debugf("%s serving request\n", enclave)

		enclave.ServeHTTP(w, r)
	})

	// Setup graceful shutdown
	server := &http.Server{
		Addr:         ":" + *port,
		Handler:      nil,             // Use default ServeMux
		ReadTimeout:  5 * time.Minute, // Increased to support large RAG payloads
		WriteTimeout: 0,               // Disabled to support long-running streaming responses
	}

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("Starting proxy server on port %s\n", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	log.Info("Shutting down server...")

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown server
	if err := server.Shutdown(ctx); err != nil {
		log.WithError(err).Error("Failed to gracefully shutdown server")
	}

	log.Info("Server stopped")
}
