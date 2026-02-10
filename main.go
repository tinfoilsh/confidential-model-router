package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
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

// getEnvBool returns true if the environment variable is set to a truthy value
func getEnvBool(envKey string) bool {
	val := strings.ToLower(os.Getenv(envKey))
	return val == "true" || val == "1" || val == "yes"
}

var (
	port            = flag.String("l", getEnvOrDefault("PORT", "8089"), "port to listen on (env: PORT)")
	controlPlaneURL = flag.String("C", getEnvOrDefault("CONTROL_PLANE_URL", "https://api.tinfoil.sh"), "control plane URL (env: CONTROL_PLANE_URL)")
	verbose         = flag.Bool("v", getEnvBool("VERBOSE"), "enable verbose logging (env: VERBOSE)")
	initConfigURL   = flag.String("i", getEnvOrDefault("INIT_CONFIG_URL", ""), "optional path to initial config.yml (requires to append @sha256:<hex> for integrity) (env: INIT_CONFIG_URL)")
	updateConfigURL = flag.String("u", getEnvOrDefault("UPDATE_CONFIG_URL", "https://raw.githubusercontent.com/tinfoilsh/confidential-model-router/main/config.yml"), "path to runtime config.yml (env: UPDATE_CONFIG_URL)")
	domain          = flag.String("d", getEnvOrDefault("DOMAIN", "localhost"), "domain used by this router (env: DOMAIN)")
)

func jsonError(w http.ResponseWriter, message string, code int) {
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
	json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

func sendJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
	}
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

func main() {
	flag.Parse()
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	log.Debugf("Configuration: domain=%s, port=%s, controlPlaneURL=%s", *domain, *port, *controlPlaneURL)

	em, err := manager.NewEnclaveManager(configFile, *controlPlaneURL, *initConfigURL, *updateConfigURL)
	if err != nil {
		log.Fatal(err)
	}
	defer em.Shutdown()
	go em.StartWorker()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var modelName string
		var err error

		if modelName, err = parseModelFromSubdomain(r, *domain); err != nil {
			jsonError(w, fmt.Sprintf("failed to parse subdomain: %v", err), http.StatusBadRequest)
			return
		} else if modelName == "" { // The request does not use a subdomain. We route using specific inference routing logic.
			if r.URL.Path == "/" {
				http.Redirect(w, r, "https://docs.tinfoil.sh", http.StatusTemporaryRedirect)
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
					jsonError(w, fmt.Sprintf("failed to create request: %v", err), http.StatusInternalServerError)
					return
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					jsonError(w, fmt.Sprintf("failed to fetch models: %v", err), http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				io.Copy(w, resp.Body)
				return
			} else if r.URL.Path == "/v1/audio/transcriptions" || strings.HasPrefix(r.URL.Path, "/v1/audio/") {
				// Extract model from multipart form, default to voxtral-small-24b
				var bodyBytes []byte
				modelName, bodyBytes, err = extractModelFromMultipart(r)
				if err != nil {
					jsonError(w, fmt.Sprintf("failed to parse request: %v", err), http.StatusBadRequest)
					return
				}
				if modelName == "" {
					modelName = "voxtral-small-24b"
				}
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			} else if r.URL.Path == "/v1/convert/file" {
				modelName = "doc-upload"
			} else { // This is an OpenAI-compatible API request
				var body map[string]interface{}
				bodyBytes, err := io.ReadAll(r.Body)

				if err != nil {
					jsonError(w, fmt.Sprintf("failed to read request body: %v", err), http.StatusBadRequest)
					return
				}
				if err := json.Unmarshal(bodyBytes, &body); err != nil {
					jsonError(w, fmt.Sprintf("failed to parse request body: %v", err), http.StatusBadRequest)
					return
				}

				// Extract model name from request body
				modelInterface, ok := body["model"]
				if !ok {
					jsonError(w, "model parameter not found in request body", http.StatusBadRequest)
					return
				}
				modelName, ok = modelInterface.(string)
				if !ok {
					jsonError(w, "model parameter must be a string", http.StatusBadRequest)
					return
				}

				// Check if request uses web search — route to websearch enclave
				// but keep modelName as the underlying model for billing.
				useWebsearch := false
				if _, ok := body["web_search_options"]; ok {
					useWebsearch = true
				} else if r.URL.Path == "/v1/responses" {
					if tools, ok := body["tools"].([]interface{}); ok {
						useWebsearch = slices.ContainsFunc(tools, func(t any) bool {
							m, _ := t.(map[string]any)
							typeVal, ok := m["type"].(string)
							return ok && typeVal == "web_search"
						})
					}
				}
				if useWebsearch {
					r = r.WithContext(context.WithValue(r.Context(), manager.RequestModelKey{}, modelName))
					modelName = "websearch"
				}

				// If streaming request, ensure continuous_usage_stats is enabled
				if stream, ok := body["stream"].(bool); ok && stream {
					// Check if client requested usage stats before we modify anything
					clientRequestedUsage := false
					if streamOptions, ok := body["stream_options"].(map[string]interface{}); ok {
						// Check for OpenAI-style include_usage
						if includeUsage, ok := streamOptions["include_usage"].(bool); ok && includeUsage {
							clientRequestedUsage = true
						}
						// Check for vLLM-style continuous_usage_stats
						if continuousUsage, ok := streamOptions["continuous_usage_stats"].(bool); ok && continuousUsage {
							clientRequestedUsage = true
						}
						streamOptions["continuous_usage_stats"] = true
					} else {
						body["stream_options"] = map[string]interface{}{
							"continuous_usage_stats": true,
						}
					}

					// Set internal header to indicate if client requested usage
					// This header will be used by the proxy to decide whether to filter usage-only chunks
					if clientRequestedUsage {
						r.Header.Set("X-Tinfoil-Client-Requested-Usage", "true")
					}

					// Re-encode the modified body
					newBodyBytes, err := json.Marshal(body)
					if err != nil {
						jsonError(w, fmt.Sprintf("failed to process request body: %v", err), http.StatusInternalServerError)
						return
					}
					bodyBytes = newBodyBytes
					// Update Content-Length header to match new body size
					r.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
					r.ContentLength = int64(len(bodyBytes))
					log.Debugf("Modified streaming request body to include continuous_usage_stats, client requested usage: %v", clientRequestedUsage)
				}

				r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}

		model, found := em.GetModel(modelName)
		if !found {
			jsonError(w, "model not found", http.StatusNotFound)
			return
		}

		enclave := model.NextEnclave()
		if enclave == nil {
			jsonError(w, "no enclaves available", http.StatusServiceUnavailable)
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

			jsonError(w, fmt.Sprintf("backend overloaded, retry after %d seconds", secs), http.StatusTooManyRequests)
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
