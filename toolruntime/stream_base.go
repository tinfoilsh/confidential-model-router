package toolruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// streamBase holds the state and helpers shared by every SSE streamer
// the router runs (currently the /chat/completions and /responses
// streamers). Both streamers embed this struct so the shared
// invariants (write-error latching, header lifecycle, usage / citation
// accumulation, upstream header capture, shared model-name validation)
// live in one place rather than being duplicated and kept in sync by
// convention.
//
// Fields that are unique to a streamer's wire format (per-chunk
// identity on /chat/completions, per-response identity plus
// output_index / sequence_number bookkeeping on /responses) stay on
// the concrete streamer types; only fields whose semantics are
// identical across both APIs live here.
type streamBase struct {
	w       http.ResponseWriter
	flusher http.Flusher

	// usageMetricsRequested reflects the UsageMetricsRequestHeader; when
	// true the streamer announces a usage-metrics trailer and sets it on
	// close.
	usageMetricsRequested bool

	citations   *citationState
	toolCalls   *toolCallLog
	usageTotals *usageAccumulator

	// headersWritten is flipped on the first 2xx from upstream; after
	// that point, errors surface as SSE error frames instead of plain
	// JSON error bodies.
	headersWritten bool

	// upstreamHeaders holds the most recent upstream response headers so
	// billing attribution matches what the non-streaming path surfaces.
	upstreamHeaders http.Header

	// model is the pinned upstream-reported model name captured on the
	// first upstream frame and reused on every subsequent client-facing
	// frame. A missing value latches writeErr via validateStreamModel.
	model string

	// writeErr latches the first client-write failure (typically
	// io.ErrClosedPipe after a client disconnect). Once set, every
	// subsequent emit call is a no-op so the pump and outer loop can
	// abort without spending more upstream tokens or MCP tool calls.
	writeErr error
}

// writeSSEHeaders sets up the shared SSE response to the client exactly
// once, the first time upstream returns a 2xx status. Identity fields
// live on the concrete streamer and are NOT pre-populated here: both
// APIs defer identity capture until the first upstream frame arrives
// so the role-delta / response.created the client sees carries
// upstream-provided ids.
func (s *streamBase) writeSSEHeaders(upstreamHeaders http.Header) {
	copyResponseHeaders(s.w.Header(), upstreamHeaders)
	s.w.Header().Del("Content-Length")
	s.w.Header().Del(manager.UsageMetricsResponseHeader)
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	if s.usageMetricsRequested {
		addTrailerHeader(s.w.Header(), manager.UsageMetricsResponseHeader)
	}
	s.w.WriteHeader(http.StatusOK)
	s.headersWritten = true
}

// validateStreamModel latches a writeErr if upstream never surfaced a
// model name by the time the streamer is about to emit a
// model-stamped frame. The field-path argument (e.g. "chunk.model" or
// "response.model") is preserved in the error so the on-call page
// points at the exact upstream field that was missing.
//
// Returns the pinned model name; callers that need to distinguish the
// empty / missing case should check writeErr after calling this.
func (s *streamBase) validateStreamModel(fieldPath string) string {
	if s.model == "" && s.writeErr == nil {
		s.writeErr = fmt.Errorf("upstream stream missing %s field", fieldPath)
	}
	return s.model
}

// newUpstreamJSONError builds a 502 *upstreamError carrying an OpenAI-shape
// {"error": errObj} body with a JSON Content-Type header, used by both
// streamers to surface malformed / truncated upstream frames.
func newUpstreamJSONError(errObj map[string]any) *upstreamError {
	return &upstreamError{
		statusCode: http.StatusBadGateway,
		header:     http.Header{"Content-Type": []string{"application/json"}},
		body:       mustMarshal(map[string]any{"error": errObj}),
	}
}

// newUpstreamStreamError is newUpstreamJSONError specialized to the
// {message, type:"upstream_error"} shape produced for protocol-level
// stream failures (malformed JSON, missing terminal marker).
func newUpstreamStreamError(message string) *upstreamError {
	return newUpstreamJSONError(map[string]any{
		"message": message,
		"type":    "upstream_error",
	})
}

// openUpstreamSSE posts the JSON-encoded reqBody to the enclave path and
// returns the SSE response body ready for sseReader consumption. On
// success it also captures the upstream headers and writes the
// client-facing SSE response headers exactly once. Non-2xx responses
// become *upstreamError so terminateWithError can surface them.
func (s *streamBase) openUpstreamSSE(
	ctx context.Context,
	em *manager.EnclaveManager,
	modelName, path string,
	reqBody map[string]any,
	requestHeaders http.Header,
) (io.ReadCloser, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	hdrs := cloneHeaders(requestHeaders)
	hdrs.Set("Content-Type", "application/json")
	hdrs.Set("Accept", "text/event-stream")

	resp, err := em.DoModelRequest(ctx, modelName, path, bodyBytes, hdrs)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &upstreamError{
			statusCode: resp.StatusCode,
			header:     resp.Header.Clone(),
			body:       errBody,
		}
	}
	s.upstreamHeaders = resp.Header
	if !s.headersWritten {
		s.writeSSEHeaders(resp.Header)
	}
	return resp.Body, nil
}

// emitBillingEvent delegates to the shared billing emitter used by the
// non-streaming path so streaming and non-streaming requests produce the
// same billing-event shape, including Tinfoil-Enclave attribution and
// request-id resolution from the most recent upstream response headers.
func (s *streamBase) emitBillingEvent(r *http.Request, em *manager.EnclaveManager, modelName string, usage map[string]any) {
	if em == nil {
		return
	}
	header := s.upstreamHeaders
	if header == nil {
		header = http.Header{}
	}
	response := &upstreamJSONResponse{
		header: header,
		body:   map[string]any{"usage": usage},
	}
	emitBillingEvent(em, r, response, modelName, true)
}

// upstreamErrorPayload extracts a structured error object to show the
// client. If the underlying error is an *upstreamError whose body is
// JSON with an `error` field, we allowlist the standard OpenAI error
// fields rather than forwarding the object verbatim.
func upstreamErrorPayload(err error) map[string]any {
	if upErr, ok := err.(*upstreamError); ok && len(upErr.body) > 0 {
		var parsed map[string]any
		if json.Unmarshal(upErr.body, &parsed) == nil {
			if inner, ok := parsed["error"].(map[string]any); ok {
				safe := map[string]any{
					"message": stringValue(inner["message"]),
					"type":    stringValue(inner["type"]),
				}
				if code, ok := inner["code"]; ok {
					safe["code"] = code
				}
				if param, ok := inner["param"]; ok {
					safe["param"] = param
				}
				return safe
			}
		}
	}
	return map[string]any{
		"message": err.Error(),
		"type":    "upstream_error",
	}
}

// mustMarshal serializes the given value to JSON, returning an empty byte
// slice on failure. Used by error-surfacing helpers where failure to
// marshal is already a lost cause; returning empty body preserves the
// status-code signal on the upstream error.
func mustMarshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}
