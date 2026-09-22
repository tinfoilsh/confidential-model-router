package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-model-router/autoroute"
	"github.com/tinfoilsh/confidential-model-router/cacheroute"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/safeguards"
	"github.com/tinfoilsh/confidential-model-router/toolruntime"
)

//go:embed config.yml
var configFile []byte // Initial (attested) config

// Set by build process
var version = "dev"

const maxRequestBodySize int64 = 64 * 1024 * 1024

// Retry-After hints, in seconds, for capacity rejections.
const (
	// modelUnavailableRetryAfterSeconds is sent when no enclave is serving
	// the model at all. Recovery depends on attestation or breaker timing,
	// so this is a floor for clients that honor the header.
	modelUnavailableRetryAfterSeconds = 30
	// defaultOverloadRetryAfterSeconds is used when the backend's queue
	// depth does not yield a retry estimate.
	defaultOverloadRetryAfterSeconds = 60
)

// cacheSaltIdentity anchors cache partitioning to an access token's claimed
// subject across token refreshes; opaque keys use the bearer itself.
//
// This is not authentication. The router's ingress shim authenticates only
// /metrics; downstream model and tool shims verify inference credentials
// before serving. A claimed subject here must not authorize inference.
func cacheSaltIdentity(apiKey string) string {
	if sub := jwtSubject(apiKey); sub != "" {
		return sub
	}
	return apiKey
}

// jwtSubject returns the `sub` claim of an explicitly typed compact-JWS access
// token, or "" if s is not an at+jwt token or carries no string subject. It
// classifies only; downstream model and tool shims verify the token before
// serving inference (see cacheSaltIdentity).
func jwtSubject(s string) string {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	if signature, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil || len(signature) == 0 {
		return ""
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	var metadata struct {
		Type string `json:"typ"`
	}
	if json.Unmarshal(header, &metadata) != nil ||
		strings.TrimPrefix(strings.ToLower(metadata.Type), "application/") != "at+jwt" {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

func limitRequestBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	if r.ContentLength > maxRequestBodySize {
		writeError(w, &manager.ErrBodyTooLarge)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	return true
}

func writeRequestBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, &manager.ErrBodyTooLarge)
		return
	}
	log.WithError(err).Warn("failed to read request body")
	writeError(w, &manager.ErrBodyReadFailed)
}

// invalidJSONError builds the error for a request body that failed JSON
// decoding. Decoder errors describe what the client sent and are passed on;
// any other failure (transport, size limit tripping mid-decode) is logged and
// reported with a fixed message.
func invalidJSONError(err error) *manager.APIError {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return &manager.ErrBodyTooLarge
	case errors.As(err, &syntaxErr), errors.As(err, &typeErr),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF),
		errors.Is(err, errBodyNotObject):
		return manager.ErrInvalidJSON.WithMessage(manager.ErrMsgInvalidJSON, err)
	}
	log.WithError(err).Warn("failed to decode request body")
	return &manager.ErrInvalidJSON
}

// writeUpstreamError normalizes a backend error response body into the
// OpenAI envelope and writes it with the backend's status. Unrecognized
// bodies are logged and replaced with the generic upstream error.
func writeUpstreamError(w http.ResponseWriter, status int, body []byte) {
	apiErr, recognized := manager.NormalizeUpstreamError(status, body)
	if !recognized {
		log.WithFields(log.Fields{
			"status": status,
			"bytes":  len(body),
		}).Warn("upstream error body is not an OpenAI error object")
	}
	writeError(w, apiErr)
}

// asAPIError returns err if it is already an APIError, otherwise wraps its
// text as a 400 invalid_request_error. Validation helpers that predate the
// APIError type still return plain errors with client-ready messages.
func asAPIError(err error) *manager.APIError {
	var apiErr *manager.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return manager.ErrInvalidRequest.WithMessage("%s", err.Error())
}

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
	port                      = flag.String("l", getEnvOrDefault("PORT", "8089"), "port to listen on (env: PORT)")
	controlPlaneURL           = flag.String("C", getEnvOrDefault("CONTROL_PLANE_URL", "https://api.tinfoil.sh"), "control plane URL (env: CONTROL_PLANE_URL)")
	usageReporterID           = flag.String("usage-reporter-id", getEnvOrDefault("USAGE_REPORTER_ID", "model-router"), "usage reporter ID (env: USAGE_REPORTER_ID)")
	usageReporterSecret       = flag.String("usage-reporter-secret", getEnvOrDefault("USAGE_REPORTER_SECRET", ""), "usage reporter HMAC secret (env: USAGE_REPORTER_SECRET)")
	usageContextSecret        = flag.String("usage-context-secret", getEnvOrDefault("USAGE_CONTEXT_SECRET", ""), "usage-context HMAC secret used to sign request-context propagated to tool services (env: USAGE_CONTEXT_SECRET)")
	inferenceDelegationSecret = flag.String("inference-delegation-secret", "", "secret used to delegate inference access tokens (env: INFERENCE_DELEGATION_SECRET)")
	verbose                   = flag.Bool("v", getEnvBool("VERBOSE"), "enable verbose logging (env: VERBOSE)")
	initConfigURL             = flag.String("i", getEnvOrDefault("INIT_CONFIG_URL", ""), "optional path to initial config.yml (requires to append @sha256:<hex> for integrity) (env: INIT_CONFIG_URL)")
	updateConfigURL           = flag.String("u", getEnvOrDefault("UPDATE_CONFIG_URL", "https://raw.githubusercontent.com/tinfoilsh/confidential-model-router/main/config.yml"), "path to runtime config.yml (env: UPDATE_CONFIG_URL)")
	refreshInterval           = flag.Duration("r", getEnvOrDefaultDuration("REFRESH_INTERVAL", 5*time.Minute), "refresh interval for syncing enclave config (env: REFRESH_INTERVAL)")
	// debug enables non-production behaviors such as honoring
	// LOCAL_MCP_ENDPOINT_<MODEL> env vars to bypass attested TLS
	// pinning for MCP tool servers during local development. MUST
	// NOT be enabled in deployed enclaves.
	debug = flag.Bool("debug", getEnvBool("DEBUG"), "enable debug-only overrides for local development (env: DEBUG)")
	// cacheSaltEnabled injects a per-principal cache_salt into supported
	// requests, partitioning the engine's prefix cache between callers.
	// Off by default for rollout; once enabled, disabling it re-opens
	// cross-user cache sharing — an emergency lever, not a tuning knob.
	cacheSaltEnabled = flag.Bool("cache-salt", getEnvBool("CACHE_SALT_ENABLED"), "inject per-principal cache_salt into requests (env: CACHE_SALT_ENABLED)")
	// cacheRouteSecret keys cache-route routing keys so key→replica
	// placement isn't computable offline by callers. Must be identical
	// across all router replicas; changing it re-homes every key. The env
	// var is resolved after parse, not as the flag default: defaults are
	// printed verbatim by -h and any flag-parse error, and the secret must
	// never reach a log.
	cacheRouteSecret = flag.String("cache-route-secret", "", "secret mixed into cache-route routing keys, must match across router replicas (env: CACHE_ROUTE_SECRET)")
	// metricsAPIKey authenticates the overload poller's enclave /metrics
	// scrapes. Env resolved after parse, like cache-route-secret, so the
	// secret never reaches flag output.
	metricsAPIKey = flag.String("metrics-api-key", "", "admin API key sent as a bearer token when polling enclave /metrics (env: METRICS_API_KEY)")
	// safeguardsURL points at the safeguards sidecar in this enclave. When
	// empty, completed first-party chat conversations are not submitted for
	// acceptable-use classification.
	safeguardsURL = flag.String("safeguards-url", getEnvOrDefault("SAFEGUARDS_URL", ""), "safeguards sidecar base URL (env: SAFEGUARDS_URL)")
)

// writeError logs the error at a level matching its status and writes it in
// the OpenAI error format. Only the stable identifiers are logged: messages
// may embed backend or document-processing text derived from request
// content, which must not leave the enclave via logs.
func writeError(w http.ResponseWriter, e *manager.APIError) {
	entry := log.WithFields(log.Fields{
		"status": e.Status,
		"type":   e.Type,
		"code":   e.Code,
		"param":  e.Param,
	})
	switch {
	case e.Status >= 500:
		entry.Error("api error")
	case e.Status >= 400:
		entry.Warn("api error")
	default:
		entry.Debug("api error")
	}
	manager.WriteAPIError(w, e)
}

func sendJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		writeError(w, &manager.ErrServer)
	}
}

// filterModelsToServed narrows an OpenAI-compatible /v1/models payload to the
// models this router actually serves, so the list reflects local availability
// rather than the control plane's full catalog. Returns an error if the payload
// can't be parsed, so the caller can surface the upstream problem.
func filterModelsToServed(body []byte, served map[string]*manager.Model) ([]byte, error) {
	var payload struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing models list: %w", err)
	}

	kept := make([]map[string]any, 0, len(payload.Data))
	for _, entry := range payload.Data {
		if id, ok := entry["id"].(string); ok {
			if _, served := served[id]; served {
				kept = append(kept, entry)
			}
		}
	}

	object := payload.Object
	if object == "" {
		object = "list"
	}
	out, err := json.Marshal(map[string]any{
		"object": object,
		"data":   kept,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding models list: %w", err)
	}
	return out, nil
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

// modelHeaderMatches reports whether the optional X-Tinfoil-Model header,
// when present, names the same model as the request body.
func modelHeaderMatches(header http.Header, bodyModel string) bool {
	expected := strings.TrimSpace(header.Get(manager.ModelRequestHeader))
	return expected == "" || expected == bodyModel
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

// autoRouteCatalog supplies the models the auto router may choose between and
// their current health. Implemented by *manager.EnclaveManager.
type autoRouteCatalog interface {
	AutoRouteCatalog() []autoroute.Model
	HasHealthyEnclave(modelName string) bool
}

// resolveAutoModel replaces model "auto" with a concrete model and reasoning
// effort. The caller's requested intelligence level (body auto_model_options,
// then the X-Tinfoil-Intelligence header, then the default) is matched against
// the per-effort scores the control plane publishes; candidates are walked in
// order of fit until one has a healthy enclave, so an outage on the best match
// degrades to the next-best rather than failing. Requests carrying images or
// files only consider multimodal models. The chosen effort is written into
// body using the model's own reasoning parameters, overriding any effort the
// client sent, and body["model"] is rewritten. When nothing is healthy the
// best-fit candidate is still returned so normal serving surfaces the error.
func resolveAutoModel(catalog autoRouteCatalog, header http.Header, path string, body map[string]any) (string, error) {
	target, err := autoroute.ParseIntelligence(header, body)
	if err != nil {
		var validationErr *autoroute.ValidationError
		if errors.As(err, &validationErr) {
			return "", manager.ErrInvalidRequest.WithParam(validationErr.Param).WithMessage("%s", validationErr.Message)
		}
		return "", err
	}

	visual := autoroute.HasVisualInput(body)
	ranked := autoroute.Rank(catalog.AutoRouteCatalog(), target, visual)
	if len(ranked) == 0 {
		if visual {
			return "", manager.ErrInvalidRequest.WithParam("model").WithMessage(manager.ErrMsgAutoNoMultimodal)
		}
		return "", manager.ErrInvalidRequest.WithParam("model").WithMessage(manager.ErrMsgAutoNoScores)
	}

	chosen, healthy := ranked[0], false
	for i, candidate := range ranked {
		if catalog.HasHealthyEnclave(candidate.Model.Name) {
			chosen, healthy = candidate, true
			if i > 0 {
				manager.AutoRouteFallbacksTotal.WithLabelValues(chosen.Model.Name).Inc()
			}
			break
		}
	}

	autoroute.ApplyEffort(body, path, chosen)
	body["model"] = chosen.Model.Name
	manager.AutoRouteDecisionsTotal.WithLabelValues(chosen.Model.Name, chosen.Effort).Inc()
	log.WithFields(log.Fields{
		"target":  target,
		"visual":  visual,
		"model":   chosen.Model.Name,
		"effort":  chosen.Effort,
		"level":   chosen.Level,
		"healthy": healthy,
	}).Debug("resolved auto model")
	return chosen.Model.Name, nil
}

func main() {
	flag.Parse()
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	log.Debugf("Configuration: port=%s, controlPlaneURL=%s", *port, *controlPlaneURL)
	log.Infof("Refresh interval: %s", *refreshInterval)

	if *usageReporterSecret == "" {
		log.Fatal("USAGE_REPORTER_SECRET is required")
	}
	if *usageContextSecret == "" {
		log.Fatal("USAGE_CONTEXT_SECRET is required")
	}
	if *inferenceDelegationSecret == "" {
		*inferenceDelegationSecret = os.Getenv("INFERENCE_DELEGATION_SECRET")
	}
	if *inferenceDelegationSecret == "" && !*debug {
		log.Fatal("INFERENCE_DELEGATION_SECRET is required")
	}

	// A routing-secret skew between replicas silently re-homes keys on only
	// the skewed instance, so tolerate the classic source — a trailing
	// newline from secret tooling — but refuse a value that is set yet
	// unusable: booting unkeyed on a garbage secret would be that same
	// silent skew.
	if secret := *cacheRouteSecret; secret != "" || os.Getenv("CACHE_ROUTE_SECRET") != "" {
		if secret == "" {
			secret = os.Getenv("CACHE_ROUTE_SECRET")
		}
		secret = strings.TrimSpace(secret)
		if len(secret) < 32 {
			log.Fatal("CACHE_ROUTE_SECRET must be at least 32 bytes (e.g. openssl rand -hex 32)")
		}
		cacheroute.SetSecret(secret)
	}

	if key := *metricsAPIKey; key != "" || os.Getenv("METRICS_API_KEY") != "" {
		if key == "" {
			key = os.Getenv("METRICS_API_KEY")
		}
		manager.SetMetricsPollAPIKey(strings.TrimSpace(key))
	}

	em, err := manager.NewEnclaveManager(configFile, *controlPlaneURL, *usageReporterID, *usageReporterSecret, *usageContextSecret, *inferenceDelegationSecret, *initConfigURL, *updateConfigURL, *refreshInterval, *debug)
	if err != nil {
		log.Fatal(err)
	}
	em.SetDebugMode(*debug)
	if *debug {
		log.Warn("debug mode enabled: local development overrides are active; do not use in production")
	}
	defer em.Shutdown()
	go em.StartWorker()

	routeContextClient := newRouteContextClient(*controlPlaneURL)

	safeguardsSubmitter := safeguards.NewSubmitter(*safeguardsURL)
	defer safeguardsSubmitter.Close()
	if safeguardsSubmitter != nil {
		log.Infof("Safeguards sidecar: %s", *safeguardsURL)
	}

	http.Handle("/", newRouterHandler(em, routeContextClient, safeguardsSubmitter))
	runRouterServer()
}

func newRouterHandler(em *manager.EnclaveManager, routeContextClient *routeContextClient, safeguardsSubmitter *safeguards.Submitter) http.Handler {
	// Measures what cache-aware replica selection would do, without
	// acting, as aggregate Prometheus metrics. Enabled per model via the
	// cache_route config block; owned by the manager so the tool loop's
	// internal dispatches are observed too.
	cacheRouteShadow := em.CacheRouteShadow()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Timestamp arrival before any parsing or routing: the first-token
		// SLA is measured from the edge of the router, so time spent on
		// body handling, route-context lookups, and replica selection is
		// part of what it reports.
		requestStart := time.Now()

		if !limitRequestBody(w, r) {
			return
		}

		var modelName string
		var err error

		// Set when the request is eligible for cache-route shadow
		// observation.
		var cacheRouteReq *cacheroute.Request
		var cacheRouteSettings cacheroute.Settings

		// Set when the control plane configured a vLLM priority for this
		// caller; such callers are exempt from overload shedding.
		hasConfiguredPriority := false

		// Set when the parsed body requests a streaming response; gates
		// the TTFT / inter-token SLA observation at dispatch.
		isStreaming := false

		// Extract API key early for rate limiting decisions
		apiKey := manager.BearerToken(r.Header.Get("Authorization"))

		// Every external inference request is admitted exactly once, after
		// the served model is known and before priority injection, forwarding,
		// or any internal dispatch. The lookup shares RPM/TPM counters across
		// the fleet, so it must not be repeated within one request. Branches
		// that read the body to learn or sanitize it do so first, then admit;
		// everything else admits after the model lookup below.
		var admission *routeContext
		admit := func() bool {
			resolved, admissionErr := routeContextClient.Lookup(r.Context(), apiKey, modelName)
			if admissionErr != nil {
				admissionErr.write(w)
				return false
			}
			admission = &resolved
			hasConfiguredPriority = resolved.overloadExempt()
			r = r.WithContext(manager.WithCallerOrg(r.Context(), resolved.OrgID))
			return true
		}

		// Completed first-party chat turns are submitted to the safeguards
		// sidecar once the response has been written.
		w, capture, finishCapture := safeguardsSubmitter.Observe(w, r, manager.IsFirstPartyChatAccessJWT)
		defer finishCapture()

		if isInputTokensPath(r.URL.Path) {
			dispatch := func(ctx context.Context, modelName, path string, body []byte, headers http.Header) (*http.Response, error) {
				if _, found := em.GetModel(modelName); !found {
					return nil, manager.ErrModelNotFound.WithMessage(manager.ErrMsgModelNotFound, modelName)
				}
				if routeCtx, lookupErr := routeContextClient.Metadata(ctx, apiKey); lookupErr == nil {
					ctx = manager.WithCallerOrg(ctx, routeCtx.OrgID)
				} else {
					return nil, lookupErr
				}
				return em.DoModelRequest(ctx, modelName, path, body, headers)
			}
			handleInputTokens(w, r, apiKey, func(body map[string]any) (string, error) {
				return resolveAutoModel(em, r.Header, inputTokensCompletionPath(r.URL.Path), body)
			}, dispatch)
			return
		}

		// WebSocket upgrade on /v1/realtime: extract model from ?model= query parameter, skip body parsing
		if isWebSocketUpgrade(r) && r.URL.Path == "/v1/realtime" {
			modelName = r.URL.Query().Get("model")
			if modelName == "" && r.URL.Query().Get("intent") == "transcription" {
				// OpenAI Realtime transcription clients connect with
				// ?intent=transcription and select the model in session.update,
				// which arrives after routing. Default to the realtime STT model.
				modelName = "voxtral-mini-4b-realtime"
			}
			if modelName == "" {
				writeError(w, manager.ErrInvalidRequest.WithParam("model").WithMessage("Missing required parameter: 'model' (use ?model=<name> query parameter for WebSocket requests)."))
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
		} else {
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
					writeError(w, &manager.ErrServer)
					return
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					writeError(w, &manager.ErrUpstream)
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					writeError(w, &manager.ErrUpstream)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				// Only a 200 is a models list we should rewrite.
				if resp.StatusCode != http.StatusOK {
					writeUpstreamError(w, resp.StatusCode, body)
					return
				}
				// A 200 we can't parse means the control plane returned
				// something unexpected — surface it rather than forwarding a
				// body we couldn't filter.
				filtered, err := filterModelsToServed(body, em.Models())
				if err != nil {
					log.Errorf("filtering /v1/models response: %v", err)
					writeError(w, &manager.ErrUpstream)
					return
				}
				w.WriteHeader(http.StatusOK)
				w.Write(filtered)
				return
			} else if r.URL.Path == "/v1/audio/speech" {
				// Extract model from JSON body, default to qwen3-tts
				var body map[string]any
				bodyBytes, err := io.ReadAll(r.Body)
				if err != nil {
					writeRequestBodyError(w, err)
					return
				}
				r.Body.Close()
				if err := json.Unmarshal(bodyBytes, &body); err != nil {
					writeError(w, invalidJSONError(err))
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
					var tooLarge *http.MaxBytesError
					if errors.As(err, &tooLarge) {
						writeRequestBodyError(w, err)
						return
					}
					writeError(w, invalidJSONError(err))
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
					writeRequestBodyError(w, err)
					return
				}
				if err := json.Unmarshal(bodyBytes, &body); err != nil {
					writeError(w, invalidJSONError(err))
					return
				}

				// Pull Tinfoil-specific options blobs off the body in
				// place. These fields (code_execution_options,
				// web_search_options, pii_check_options) are
				// router-only.
				routerOpts, err := toolruntime.ExtractRouterOptions(body)
				if err != nil {
					writeError(w, asAPIError(err))
					return
				}

				// Extract model name from request body
				modelInterface, ok := body["model"]
				if !ok {
					writeError(w, manager.ErrInvalidRequest.WithParam("model").WithMessage(manager.ErrMsgMissingParam, "model"))
					return
				}
				modelName, ok = modelInterface.(string)
				if !ok || strings.TrimSpace(modelName) == "" {
					writeError(w, manager.ErrInvalidRequest.WithParam("model").WithMessage(manager.ErrMsgInvalidParam, "model", "must be a string"))
					return
				}
				if !modelHeaderMatches(r.Header, modelName) {
					writeError(w, &manager.ErrModelMismatch)
					return
				}

				// "auto" is a router-side sentinel: the caller states an
				// intelligence level and the router picks a concrete,
				// healthy model and reasoning effort for it before any
				// downstream logic (tool detection, rate limiting, serving)
				// runs.
				if modelName == "auto" {
					resolved, resolveErr := resolveAutoModel(em, r.Header, r.URL.Path, body)
					if resolveErr != nil {
						writeError(w, asAPIError(resolveErr))
						return
					}
					modelName = resolved
				}

				if capture != nil {
					capture.SetMessages(safeguards.RequestMessages(r.URL.Path, body))
				}

				// Detect which built-in tool profiles this request
				// activates. The router runs the tool loop locally
				// against one MCP session per active profile; zero
				// profiles and no auto-continue tools means no router-owned
				// work, so the request falls through to the plain proxy path.
				activeProfiles := toolruntime.DetectProfiles(r.URL.Path, routerOpts, body)
				hasAutoContinueTools := toolruntime.HasAutoContinueTools(r.URL.Path, body)

				// Strip any user-supplied priority to prevent circumventing rate limits
				// or jumping ahead of other users.
				delete(body, "priority")

				// Admit before any internal dispatch (file conversion, tool
				// loop) so they all reuse this request's context and
				// reservation pools. Unknown models must 404 without being
				// counted against anyone's quota.
				if _, found := em.GetModel(modelName); !found {
					em.ReportUnknownModel(apiKey, modelName)
					writeError(w, manager.ErrModelNotFound.WithMessage(manager.ErrMsgModelNotFound, modelName))
					return
				}
				if !admit() {
					return
				}
				admission.applyPriority(body, r.URL.Path, modelName)

				if r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/chat/completions" {
					switch r.URL.Path {
					case "/v1/responses":
						_, err = rewriteResponsesBase64Files(r.Context(), body, em, r.Header.Get("Authorization"), modelName)
					case "/v1/chat/completions":
						_, err = rewriteChatCompletionsBase64Files(r.Context(), body, em, r.Header.Get("Authorization"), modelName)
					}
					if err != nil {
						var apiErr *manager.APIError
						if errors.As(err, &apiErr) {
							writeError(w, apiErr)
							return
						}

						writeError(w, &manager.ErrUpstream)
						return
					}
				}

				// Own the cache-salt fields: pop the router-only
				// user_cache_secret, strip any client-supplied cache_salt,
				// and (when enabled) inject the derived per-principal salt
				// on endpoints that support it.
				mode, _ := applyCacheSalt(body, r.URL.Path, apiKey, *cacheSaltEnabled)
				recordCacheSaltInjection(modelName, mode)

				// If streaming request, ensure upstream usage is available for billing.
				if stream, ok := body["stream"].(bool); ok && stream {
					isStreaming = true
					ensureStreamingUsageOptions(body, r.Header)
					log.Debugf("Modified streaming request body to include usage for billing, client requested usage: %v",
						r.Header.Get("X-Tinfoil-Client-Requested-Usage") == "true")
				}

				// Always re-marshal in case there were any changes
				bodyBytes, err = json.Marshal(body)
				if err != nil {
					writeError(w, &manager.ErrServer)
					return
				}
				r.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
				r.ContentLength = int64(len(bodyBytes))

				r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

				if len(activeProfiles) > 0 || hasAutoContinueTools {
					// Streaming tool requests feed the same first-token SLA
					// metrics as plain proxied streams. The loop may dispatch
					// to several replicas and pools before the first
					// client-visible token, so enclave and pool carry the
					// tool-runtime sentinel; the deferred finish still counts
					// requests that end token-less, including a panic unwind.
					tw := http.ResponseWriter(w)
					toolServed := false
					if isStreaming && latencyMetricPaths[r.URL.Path] {
						lw := newToolLatencyWriter(w, requestStart, modelName, priorityClass(hasConfiguredPriority))
						tw = lw
						defer func() {
							lw.aborted = !toolServed
							lw.finish(r.Context())
							observeRequestDuration(r.Context(), modelName, toolRuntimeLabel, lw.class, true, lw.status, lw.aborted, requestStart)
						}()
					} else {
						defer func() {
							observeRequestDuration(r.Context(), modelName, toolRuntimeLabel, priorityClass(hasConfiguredPriority), false, 0, !toolServed, requestStart)
						}()
					}
					if err := toolruntime.Handle(tw, r, em, activeProfiles, body, modelName, routerOpts); err != nil {
						log.WithError(err).WithFields(log.Fields{
							"model": modelName,
							"path":  r.URL.Path,
						}).Error("tool runtime failed")
						// Once SSE headers are on the wire the client already
						// holds a 200; the failure was reported in-band and a
						// JSON error here would corrupt the event stream.
						var aborted *toolruntime.StreamAbortedError
						if !errors.As(err, &aborted) {
							writeError(tw, &manager.ErrUpstream)
						}
					}
					toolServed = true
					return
				}

				// Derive the cache-route shadow key now that the body is
				// final (post file-input rewriting, post salt injection)
				// and the request is known to take the plain proxy path —
				// the tool runtime above observes its own dispatches via
				// DoModelRequestJSON. Observed after replica selection
				// below; never fails the request.
				if cacheSaltPaths[r.URL.Path] {
					if m, ok := em.GetModel(modelName); ok {
						if s := m.CacheRouteSettings(); s.Mode != cacheroute.ModeOff {
							salt, _ := body["cache_salt"].(string)
							cacheRouteSettings = s
							cacheRouteReq = cacheroute.ExtractRequest(body, r.URL.Path, salt, s)
						}
					}
				}
			}
		}

		model, found := em.GetModel(modelName)
		if !found {
			em.ReportUnknownModel(apiKey, modelName)
			writeError(w, manager.ErrModelNotFound.WithMessage(manager.ErrMsgModelNotFound, modelName))
			return
		}

		// Requests whose body was not parsed above (audio, file conversion,
		// and realtime) are admitted here, after the authoritative model lookup.
		if admission == nil {
			if !admit() {
				return
			}
			// Speech carries a JSON object the engine accepts a
			// priority field on, so a client-supplied value must be stripped
			// like on the parsed paths. Other endpoints may carry JSON-RPC
			// batches, compressed payloads, or opaque file data and are
			// proxied verbatim.
			if !isWebSocketUpgrade(r) && r.URL.Path == "/v1/audio/speech" {
				body, _, err := saltProxiedBody(r, apiKey, *cacheSaltEnabled)
				if err != nil {
					writeError(w, invalidJSONError(err))
					return
				}
				admission.applyPriority(body, r.URL.Path, modelName)
				if err := replaceJSONBody(r, body); err != nil {
					writeError(w, &manager.ErrServer)
					return
				}
			}
		}

		// Reuse the admission context for reservation selection.
		var poolPrimary, poolSpill map[string]bool
		if model.HasReservations() {
			poolPrimary, poolSpill = model.ReservationPools(admission.OrgID)
		}

		// On enforced pools, keyed requests are served in cache-aware
		// preference order: the key's pick first, then the rest of its
		// ranking, so an overloaded pick spills to the next-warmest host
		// via the skip loop below instead of a random sibling. The
		// decision carries its pool snapshot and replication factor to
		// the landing observation, so the metrics describe the decision
		// that actually routed the request. The pool is scoped to the
		// caller's primary reservation pool.
		var cacheRouteDecision *cacheroute.Decision
		var cacheRouteOrder []string
		if cacheRouteReq != nil && cacheRouteSettings.Mode == cacheroute.ModeEnforced {
			cacheRouteDecision = cacheRouteShadow.Decide(modelName, cacheRouteReq, model.CacheRoutePoolIn(poolPrimary), cacheRouteSettings)
			if cacheRouteDecision != nil {
				cacheRouteOrder = cacheRouteDecision.Order
			}
		}

		var (
			enclave    *manager.Enclave
			probeClaim *manager.ProbeClaim
			overloaded bool
			retryAfter time.Duration
			waiting    float64
		)
		if hasConfiguredPriority {
			// Configured-priority callers have no 429 path: like internal
			// dispatches they serve through overload at the warmest host,
			// where their injected priority jumps the backend queue.
			enclave, probeClaim = model.SelectForDispatchPools(cacheRouteOrder, poolPrimary, poolSpill)
			if enclave != nil && probeClaim == nil {
				if ov, _, _ := enclave.ShouldReject(); ov {
					manager.PriorityOverloadAdmitsTotal.WithLabelValues(modelName).Inc()
				}
			}
		} else {
			enclave, probeClaim, overloaded, retryAfter, waiting = model.SelectServing(cacheRouteOrder, poolPrimary, poolSpill)
		}
		if enclave == nil {
			w.Header().Set("Retry-After", strconv.Itoa(modelUnavailableRetryAfterSeconds))
			writeError(w, manager.ErrModelUnavailable.WithMessage(manager.ErrMsgModelUnavailable, modelName))
			return
		}

		// Meter landing pools on reserved models; poolSpill is only
		// non-nil for reserved callers. Models without reservations have
		// no pool concept and carry "none" on the SLA metrics.
		poolLabel := "none"
		if poolPrimary != nil && !overloaded {
			poolLabel = "shared"
			if poolSpill != nil {
				if poolPrimary[enclave.String()] {
					poolLabel = "reserved"
				} else {
					poolLabel = "spilled"
					log.WithFields(log.Fields{
						"model":   modelName,
						"enclave": enclave.String(),
					}).Debug("reserved pool exhausted; spilling to shared pool")
				}
			}
			manager.ReservedPoolServesTotal.WithLabelValues(modelName, poolLabel).Inc()
		}

		if overloaded {
			secs := int(retryAfter.Seconds())
			if secs <= 0 {
				secs = defaultOverloadRetryAfterSeconds
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			fields := log.Fields{
				"model":               modelName,
				"enclave":             enclave.String(),
				"requests_waiting":    waiting,
				"retry_after_seconds": secs,
			}
			// With hysteresis, requests_waiting can sit below the trip mark
			// while the queue drains; log the marks so an in-band reject
			// doesn't read as a contradiction.
			if trip, clear, ok := enclave.OverloadMarks(); ok {
				fields["max_requests_waiting"] = trip
				fields["clear_requests_waiting"] = clear
			}
			log.WithFields(fields).Warn("rejecting request due to backend overload")

			// Record rejection metrics
			manager.RequestsRejectedTotal.WithLabelValues(modelName).Inc()
			manager.RetryAfterSeconds.WithLabelValues(modelName).Observe(float64(secs))

			writeError(w, manager.ErrServerOverloaded.WithMessage(manager.ErrMsgOverloaded, modelName, secs))
			return
		}

		log.Debugf("%s serving request\n", enclave)

		// Everything between arrival and here — body handling, route-context
		// lookup, rate limiting, selection — is router overhead; isolate it
		// so backend latency and router latency stay attributable.
		manager.DispatchSeconds.WithLabelValues(modelName).Observe(time.Since(requestStart).Seconds())

		// Hand the landing to the cache-route pipeline. Observed at
		// dispatch so the picked replica counts as warm from prefill
		// start; cannot affect the request.
		if cacheRouteDecision != nil {
			cacheRouteShadow.ObserveLanding(modelName, cacheRouteReq, cacheRouteDecision, enclave.String(), cacheRouteSettings)
		} else if cacheRouteReq != nil {
			cacheRouteShadow.Observe(modelName, cacheRouteReq, model.CacheRoutePoolIn(poolPrimary), enclave.String(), cacheRouteSettings)
		}

		// A claimed recovery probe travels with its request, so the
		// enclave's outcome handlers can tell an owner's cancellation
		// (which must release the claim) from an unrelated one.
		if probeClaim != nil {
			r = r.WithContext(manager.WithProbeClaim(r.Context(), probeClaim))
		}

		// Per-request token counts surface only in the proxy's usage
		// handler; carry the labels resolved here to it.
		r = r.WithContext(manager.WithTokenMetricLabels(r.Context(), poolLabel, priorityClass(hasConfiguredPriority)))

		if isStreaming && latencyMetricPaths[r.URL.Path] {
			lw := newLatencyWriter(w, requestStart, modelName, enclave.String(), poolLabel, priorityClass(hasConfiguredPriority))
			// finish must run via defer: a backend that dies mid-stream
			// unwinds ServeHTTP with http.ErrAbortHandler, and that request
			// must still be counted as failed-before-first-token. served
			// distinguishes that unwind from a normal return.
			served := false
			defer func() {
				lw.aborted = !served
				lw.finish(r.Context())
				observeRequestDuration(r.Context(), modelName, poolLabel, lw.class, true, lw.status, lw.aborted, requestStart)
			}()
			enclave.ServeHTTP(lw, r)
			served = true
			return
		}
		// WebSocket sessions are hijacked connections whose lifetime is the
		// session, not a request — a duration observation there would be
		// noise, so they proxy unwrapped.
		if isWebSocketUpgrade(r) {
			enclave.ServeHTTP(w, r)
			return
		}
		served := false
		defer func() {
			observeRequestDuration(r.Context(), modelName, poolLabel, priorityClass(hasConfiguredPriority), false, 0, !served, requestStart)
		}()
		enclave.ServeHTTP(w, r)
		served = true
	})
}

func runRouterServer() {
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
