package toolruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
	"github.com/tinfoilsh/confidential-model-router/toolcontext"
	"github.com/tinfoilsh/confidential-model-router/toolprofile"
)

// ---------------------------------------------------------------------------
// Types (grouped with their methods)
// ---------------------------------------------------------------------------

type upstreamJSONResponse struct {
	body       map[string]any
	header     http.Header
	statusCode int
}

type upstreamError struct {
	statusCode int
	header     http.Header
	body       []byte
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d: %s", e.statusCode, strings.TrimSpace(string(e.body)))
}

type usageAccumulator struct {
	promptTokens     int
	completionTokens int
	totalTokens      int
}

func (a *usageAccumulator) Add(response *upstreamJSONResponse) {
	usage := usageFromRaw(response.body["usage"])
	if usage == nil {
		return
	}

	a.promptTokens += usage.PromptTokens
	a.completionTokens += usage.CompletionTokens
	if usage.TotalTokens > 0 {
		a.totalTokens += usage.TotalTokens
		return
	}
	a.totalTokens += usage.PromptTokens + usage.CompletionTokens
}

func (a *usageAccumulator) Usage() *tokencount.Usage {
	if a.promptTokens == 0 && a.completionTokens == 0 && a.totalTokens == 0 {
		return nil
	}

	totalTokens := a.totalTokens
	if totalTokens == 0 {
		totalTokens = a.promptTokens + a.completionTokens
	}

	return &tokencount.Usage{
		PromptTokens:     a.promptTokens,
		CompletionTokens: a.completionTokens,
		TotalTokens:      totalTokens,
		InputTokens:      a.promptTokens,
		OutputTokens:     a.completionTokens,
	}
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	for key, values := range t.headers {
		for _, value := range values {
			if value != "" {
				cloned.Header.Add(key, value)
			}
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// Handle routes a request through the shared MCP tool loop against
// one or more active tool profiles. Every profile in the slice is
// dialed once, its tools are merged into a sessionRegistry, and
// router-owned tool calls are dispatched by tool name to the
// session that advertised them. Passing multiple profiles is the
// path for requests that activate more than one built-in tool at
// once.
func Handle(w http.ResponseWriter, r *http.Request, em *manager.EnclaveManager, profiles []toolprofile.Profile, body map[string]any, modelName string) error {
	ctx := r.Context()
	requestHeaders := modelRequestHeaders(r.Header)
	usageMetricsRequested := r.Header.Get(manager.UsageMetricsRequestHeader) == "true"
	eventFlags := parseTinfoilEventFlags(r.Header)
	safetyOpts := parseSafetyOptIns(body)

	dial := func(ctx context.Context, p toolprofile.Profile) (*mcp.ClientSession, error) {
		return connectToolSession(ctx, em, p, r, modelName, body, safetyOpts)
	}
	registry, err := buildSessionRegistry(ctx, profiles, dial)
	if err != nil {
		return err
	}
	defer registry.CloseAll()

	var dl *devLog
	if em.DebugMode() {
		dl = openDevLog(r, body, modelName, registry)
		defer dl.Close()
	}

	harmony := isHarmonyModel(modelName)
	promptResult := buildRouterPrompt(harmony, registry.profileNames())

	streaming := isStream(body)
	switch r.URL.Path {
	case "/v1/chat/completions":
		if streaming {
			if err := runChatStreaming(ctx, w, r, em, registry, body, modelName, requestHeaders, promptResult, dl); err != nil {
				return writeUpstreamError(w, err)
			}
			return nil
		}
		response, err := runChatLoop(ctx, em, registry, body, modelName, requestHeaders, promptResult, eventFlags, harmony, dl)
		if err != nil {
			return writeUpstreamError(w, err)
		}
		applyUsageMetrics(response, usageMetricsRequested)
		emitBillingEvent(em, r, response, modelName, false)
		return writeJSONResponse(w, response)
	case "/v1/responses":
		if streaming {
			if err := runResponsesStreaming(ctx, w, r, em, registry, body, modelName, requestHeaders, promptResult, dl); err != nil {
				return writeUpstreamError(w, err)
			}
			return nil
		}
		response, err := runResponsesLoop(ctx, em, registry, body, modelName, requestHeaders, promptResult, eventFlags, harmony, dl)
		if err != nil {
			return writeUpstreamError(w, err)
		}
		applyUsageMetrics(response, usageMetricsRequested)
		emitBillingEvent(em, r, response, modelName, false)
		return writeJSONResponse(w, response)
	default:
		return fmt.Errorf("unsupported tool runtime route: %s", r.URL.Path)
	}
}

// ---------------------------------------------------------------------------
// Session setup
// ---------------------------------------------------------------------------

// connectToolSession opens one attested MCP session for a single
// profile. buildSessionRegistry invokes it once per active profile
// and aggregates the results into the per-request routing table.
func connectToolSession(ctx context.Context, em *manager.EnclaveManager, profile toolprofile.Profile, r *http.Request, modelName string, body map[string]any, safety safetyOptIns) (*mcp.ClientSession, error) {
	endpoint, httpClient, err := em.MCPServerEndpoint(profile.ToolServerModel)
	if err != nil {
		return nil, err
	}

	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = uuid.NewString()
	}

	headers := make(http.Header)
	headers.Set(toolcontext.HeaderRequestID, requestID)
	headers.Set(toolcontext.HeaderModel, modelName)
	headers.Set(toolcontext.HeaderRoute, r.URL.Path)
	headers.Set(toolcontext.HeaderStreaming, strconv.FormatBool(isStream(body)))
	if safety.pii != nil {
		headers.Set(toolcontext.HeaderPIICheck, strconv.FormatBool(*safety.pii))
	}
	if safety.injection != nil {
		headers.Set(toolcontext.HeaderInjectionCheck, strconv.FormatBool(*safety.injection))
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		headers.Set("Authorization", auth)
	}
	if sessionID := r.Header.Get("X-Session-Id"); sessionID != "" {
		headers.Set("X-Session-Id", sessionID)
	}
	httpClient.Transport = &headerRoundTripper{
		base:    httpClient.Transport,
		headers: headers,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "router-tool-runtime", Version: "v1"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	}, nil)
}

// ---------------------------------------------------------------------------
// Loop wrappers
// ---------------------------------------------------------------------------

func runChatLoop(ctx context.Context, em *manager.EnclaveManager, registry *sessionRegistry, body map[string]any, modelName string, requestHeaders http.Header, prompt *mcp.GetPromptResult, eventFlags tinfoilEventFlags, harmony bool, dl *devLog) (*upstreamJSONResponse, error) {
	adapter := newChatLoopAdapter(body, prompt, registry.allTools(), registry.ownedTools(), modelName, requestHeaders)
	return runToolLoop(ctx, em, registry, modelName, requestHeaders, adapter, eventFlags, harmony, dl)
}

func runResponsesLoop(ctx context.Context, em *manager.EnclaveManager, registry *sessionRegistry, body map[string]any, modelName string, requestHeaders http.Header, prompt *mcp.GetPromptResult, eventFlags tinfoilEventFlags, harmony bool, dl *devLog) (*upstreamJSONResponse, error) {
	adapter := newResponsesLoopAdapter(body, prompt, registry.allTools(), registry.ownedTools())
	return runToolLoop(ctx, em, registry, modelName, requestHeaders, adapter, eventFlags, harmony, dl)
}

// ---------------------------------------------------------------------------
// Upstream communication
// ---------------------------------------------------------------------------

func postJSON(ctx context.Context, em *manager.EnclaveManager, modelName, path string, body map[string]any, requestHeaders http.Header) (*upstreamJSONResponse, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	reqHeaders := cloneHeaders(requestHeaders)
	reqHeaders.Set("Content-Type", "application/json")

	resp, err := em.DoModelRequest(ctx, modelName, path, bodyBytes, reqHeaders)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &upstreamError{
			statusCode: resp.StatusCode,
			header:     resp.Header.Clone(),
			body:       respBody,
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	return &upstreamJSONResponse{
		body:       parsed,
		header:     resp.Header.Clone(),
		statusCode: resp.StatusCode,
	}, nil
}

// ---------------------------------------------------------------------------
// Response writing
// ---------------------------------------------------------------------------

func writeJSONResponse(w http.ResponseWriter, response *upstreamJSONResponse) error {
	data, err := json.Marshal(response.body)
	if err != nil {
		return err
	}

	copyResponseHeaders(w.Header(), response.header)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(response.statusCode)
	_, err = w.Write(data)
	return err
}

func writeUpstreamError(w http.ResponseWriter, err error) error {
	upstreamErr, ok := err.(*upstreamError)
	if !ok {
		return err
	}

	copyResponseHeaders(w.Header(), upstreamErr.header)
	if contentType := upstreamErr.header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(upstreamErr.body)))
	w.WriteHeader(upstreamErr.statusCode)
	_, writeErr := w.Write(upstreamErr.body)
	return writeErr
}

// ---------------------------------------------------------------------------
// SSE helpers
// ---------------------------------------------------------------------------

func sseData(w http.ResponseWriter, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func sseEvent(w http.ResponseWriter, event string, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

// ---------------------------------------------------------------------------
// Usage / billing
// ---------------------------------------------------------------------------

func usageFromRaw(raw any) *tokencount.Usage {
	if raw == nil {
		return nil
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var usage tokencount.Usage
	if err := json.Unmarshal(data, &usage); err != nil {
		return nil
	}
	usage.Normalize()
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		return nil
	}
	return &usage
}

func formatUsageHeader(usage *tokencount.Usage) string {
	return "prompt=" + strconv.Itoa(usage.PromptTokens) +
		",completion=" + strconv.Itoa(usage.CompletionTokens) +
		",total=" + strconv.Itoa(usage.TotalTokens)
}

func applyUsageMetrics(response *upstreamJSONResponse, usageMetricsRequested bool) {
	if response == nil {
		return
	}

	response.header.Del(manager.UsageMetricsResponseHeader)
	if !usageMetricsRequested {
		return
	}

	usage := usageFromRaw(response.body["usage"])
	if usage == nil {
		return
	}
	response.header.Set(manager.UsageMetricsResponseHeader, formatUsageHeader(usage))
}

func emitBillingEvent(em *manager.EnclaveManager, r *http.Request, response *upstreamJSONResponse, modelName string, streaming bool) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return
	}

	apiKey := strings.TrimPrefix(authHeader, "Bearer ")
	if apiKey == "" {
		return
	}

	usage := usageFromRaw(response.body["usage"])
	promptTokens := 0
	completionTokens := 0
	totalTokens := 0
	if usage != nil {
		promptTokens = usage.PromptTokens
		completionTokens = usage.CompletionTokens
		totalTokens = usage.TotalTokens
	}

	em.AddBillingEvent(billing.Event{
		Timestamp:        time.Now(),
		UserID:           "authenticated_user",
		APIKey:           apiKey,
		Model:            modelName,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		RequestID:        responseRequestID(response.header, r.Header),
		Enclave:          response.header.Get("Tinfoil-Enclave"),
		RequestPath:      r.URL.Path,
		Streaming:        streaming,
	})
}

func responseRequestID(headers ...http.Header) string {
	for _, header := range headers {
		if header == nil {
			continue
		}
		if requestID := header.Get("X-Request-Id"); requestID != "" {
			return requestID
		}
		if requestID := header.Get("X-Request-ID"); requestID != "" {
			return requestID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// HTTP header helpers
// ---------------------------------------------------------------------------

func modelRequestHeaders(source http.Header) http.Header {
	headers := make(http.Header)
	for key, values := range source {
		if shouldSkipRequestHeader(key) {
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		headers[key] = copied
	}
	return headers
}

func shouldSkipRequestHeader(key string) bool {
	canonical := http.CanonicalHeaderKey(key)
	switch canonical {
	case "Accept-Encoding", "Connection", "Content-Length", "Content-Type", "Host", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return canonical == http.CanonicalHeaderKey(manager.UsageMetricsRequestHeader) ||
		canonical == http.CanonicalHeaderKey("X-Tinfoil-Client-Requested-Usage")
}

func cloneHeaders(source http.Header) http.Header {
	headers := make(http.Header, len(source))
	for key, values := range source {
		copied := make([]string, len(values))
		copy(copied, values)
		headers[key] = copied
	}
	return headers
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if shouldSkipResponseHeader(key) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldSkipResponseHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func addTrailerHeader(h http.Header, name string) {
	existing := h.Values("Trailer")
	for _, value := range existing {
		for _, part := range strings.Split(value, ",") {
			if http.CanonicalHeaderKey(strings.TrimSpace(part)) == http.CanonicalHeaderKey(name) {
				return
			}
		}
	}

	if len(existing) == 0 {
		h.Set("Trailer", name)
		return
	}

	h.Set("Trailer", strings.Join(append(existing, name), ", "))
}

// ---------------------------------------------------------------------------
// Value helpers
// ---------------------------------------------------------------------------

func isStream(body map[string]any) bool {
	stream, _ := body["stream"].(bool)
	return stream
}

func cloneJSONMap(in map[string]any) map[string]any {
	data, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if s := stringValue(value); s != "" {
			return s
		}
	}
	return ""
}

func numberValue(v any) float64 {
	f, _ := v.(float64)
	return f
}
