package toolruntime

import (
	"fmt"
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
	// w is the client-facing response writer. All client-bound SSE
	// frames flow through it, and its underlying connection is what
	// client disconnect / writeErr latching protects.
	w http.ResponseWriter

	// flusher flushes the response after every frame so clients see
	// deltas as soon as upstream produces them rather than waiting for
	// Go's default response buffering.
	flusher http.Flusher

	// usageMetricsRequested reflects the UsageMetricsRequestHeader the
	// upstream proxy forwarded: when true the streamer announces a
	// usage-metrics trailer and sets it on close.
	usageMetricsRequested bool

	// citations holds the per-request citation state (numbering,
	// resolved URL metadata). It is shared with the tool runtime so
	// web-search tool calls and streamed text annotations see the same
	// citation cursor.
	citations *citationState

	// usageTotals aggregates usage across every upstream iteration of
	// the logical stream. Finalize uses it to emit a single authoritative
	// usage block that reflects the whole response, not just the last
	// turn.
	usageTotals *usageAccumulator

	// headersWritten is true once the SSE response headers have been
	// emitted. After that, upstream errors surface as SSE error frames
	// rather than plain JSON error bodies.
	headersWritten bool

	// upstreamHeaders holds the most recent upstream response headers
	// so the billing event and any passthrough headers (enclave
	// attribution, request ids) match what the non-streaming path
	// surfaces.
	upstreamHeaders http.Header

	// model is the pinned upstream-reported model name captured on the
	// first upstream frame and reused on every subsequent client-facing
	// frame. It is stored here because both streamers validate it the
	// same way (missing -> upstream bug, latch writeErr) even though
	// the upstream frames that carry it are structured differently.
	model string

	// writeErr latches the first error observed while writing to the
	// client SSE stream (typically io.ErrClosedPipe after a client
	// disconnect, or an upstream-bug sentinel from streamModel when
	// the model field is missing). Once set, every subsequent emit
	// call is a no-op so the pump and outer loop can notice the
	// failure and abort without spending more upstream tokens or MCP
	// tool calls on a caller that has gone away.
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
