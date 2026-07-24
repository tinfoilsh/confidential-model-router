package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// latencyMetricPaths are the endpoints where streaming TTFT and inter-token
// gaps are meaningful SLA signals.
var latencyMetricPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/responses":        true,
}

func priorityClass(hasConfiguredPriority bool) string {
	if hasConfiguredPriority {
		return "configured"
	}
	return "none"
}

// latencyWriter wraps http.ResponseWriter to observe streaming latency:
// legacy time-to-first-byte, inter-chunk gaps, and — via an incremental SSE
// scan of the bytes actually sent to the client — time to the first
// generated token, measured from request arrival at the router. Only 2xx
// responses are observed, so proxy error bodies don't register as fast first
// tokens. It deliberately does not implement io.ReaderFrom: a delegating
// ReadFrom would bypass Write and drop the per-chunk timing.
type latencyWriter struct {
	http.ResponseWriter
	model   string
	enclave string
	pool    string
	class   string
	arrival time.Time // request arrival at the router: first-token zero point
	start   time.Time // dispatch to the backend: legacy first-byte zero point
	status  int
	last    time.Time
	nowFunc func() time.Time

	// observeLegacy feeds the dispatch-scoped TTFTSeconds,
	// ReplicaTTFTSeconds, and InterTokenSeconds metrics. Only the plain proxy
	// path sets it: those series predate first-token detection, and widening
	// their population (e.g. with tool-runtime streams) would silently shift
	// dashboards that still read them.
	observeLegacy bool

	detector  *sseTokenDetector // non-nil while scanning an SSE body for the first token
	tokenSeen bool
	aborted   bool // ServeHTTP unwound by panic: the proxy aborted mid-stream
}

func newLatencyWriter(w http.ResponseWriter, arrival time.Time, model, enclave, pool, class string) *latencyWriter {
	return &latencyWriter{
		ResponseWriter: w,
		model:          model,
		enclave:        enclave,
		pool:           pool,
		class:          class,
		arrival:        arrival,
		start:          time.Now(),
		observeLegacy:  true,
	}
}

// toolRuntimeLabel marks first-token series for requests served by the
// router's tool loop rather than one proxied replica. The loop may dispatch
// to several replicas and pools before the first client-visible token, so
// no single enclave or pool value would be truthful — and the sentinel keeps
// tool traffic separable from per-replica latencies on dashboards.
const toolRuntimeLabel = "tool-runtime"

// newToolLatencyWriter observes first-token latency for SSE streams the
// tool runtime writes to the client. The first client-visible output on
// this path may be inline tool-progress content rather than model text;
// both are generated stream output the caller is waiting on, and counting
// only the final answer would misread a request the user is actively
// watching as an SLA violation.
func newToolLatencyWriter(w http.ResponseWriter, arrival time.Time, model, class string) *latencyWriter {
	return &latencyWriter{
		ResponseWriter: w,
		model:          model,
		enclave:        toolRuntimeLabel,
		pool:           toolRuntimeLabel,
		class:          class,
		arrival:        arrival,
		start:          time.Now(),
	}
}

func (lw *latencyWriter) now() time.Time {
	if lw.nowFunc != nil {
		return lw.nowFunc()
	}
	return time.Now()
}

func (lw *latencyWriter) WriteHeader(code int) {
	// 1xx headers are interim (the proxy forwards them ahead of the real
	// status), so keep updating until the first non-informational code.
	if lw.status < 200 {
		lw.status = code
	}
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *latencyWriter) Write(b []byte) (int, error) {
	// status 0 means an implicit 200 from a Write without WriteHeader
	observe := lw.status < 300
	var now time.Time
	if observe {
		now = lw.now()
		if lw.last.IsZero() {
			if lw.observeLegacy {
				ttft := now.Sub(lw.start).Seconds()
				manager.TTFTSeconds.WithLabelValues(lw.model, lw.class).Observe(ttft)
				manager.ReplicaTTFTSeconds.WithLabelValues(lw.model, lw.enclave, lw.pool, lw.class).Observe(ttft)
			}
			if isEventStream(lw.Header().Get("Content-Type")) {
				lw.detector = &sseTokenDetector{}
			}
		} else if lw.observeLegacy {
			manager.InterTokenSeconds.WithLabelValues(lw.model, lw.class).Observe(now.Sub(lw.last).Seconds())
		}
		lw.last = now
	}

	n, err := lw.ResponseWriter.Write(b)

	// First-token detection runs after the delegated write and only on
	// success: a chunk the connection refused was never sent to the client,
	// and recording it would both fake a served token and let finish() skip
	// the request's no-token count. err == nil is the strongest delivery
	// signal this layer has (and implies n == len(b) per the io.Writer
	// contract). On failure the proxy aborts the stream, so the unfed
	// detector state is never consulted again.
	if observe && err == nil && !lw.tokenSeen {
		if lw.detector == nil {
			// The backend answered the streaming request with a plain 2xx
			// body: the whole payload is the generation, so its first
			// non-empty write is the first token.
			if n > 0 {
				lw.observeFirstToken(now)
			}
		} else if lw.detector.feed(b) {
			lw.observeFirstToken(now)
		}
	}
	return n, err
}

// isEventStream reports whether a Content-Type header declares an SSE body.
// Media types are case-insensitive (RFC 9110 §8.3.1) and a substring match
// would also hit unrelated types that merely embed this one, so parse
// properly: misreading the type silently reverts first-token detection to
// first-byte semantics. A malformed optional parameter doesn't obscure the
// type itself, so it doesn't disqualify the stream.
func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) {
		return false
	}
	return mediaType == "text/event-stream"
}

func (lw *latencyWriter) observeFirstToken(now time.Time) {
	lw.tokenSeen = true
	lw.detector = nil
	manager.FirstTokenSeconds.WithLabelValues(lw.model, lw.enclave, lw.pool, lw.class).
		Observe(now.Sub(lw.arrival).Seconds())
}

// finish counts a request that ended before any generated token was sent.
// It must run deferred, not inline after ServeHTTP: a backend that dies
// mid-stream unwinds the handler with http.ErrAbortHandler, and that
// request must still be counted.
func (lw *latencyWriter) finish(ctx context.Context) {
	if lw.tokenSeen {
		return
	}
	var reason string
	switch {
	// A client disconnect both cancels the request context and aborts the
	// proxy copy; classify on the context first so client-fault outcomes
	// aren't misfiled as backend errors. (The proxy's courtesy 502 after a
	// cancellation is likewise outranked here.)
	case errors.Is(ctx.Err(), context.Canceled):
		reason = "canceled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		reason = "timeout"
	case lw.status >= 300,
		lw.aborted,
		lw.detector != nil && lw.detector.sawError:
		reason = "error"
	default:
		reason = "no_output"
	}
	manager.NoFirstTokenTotal.WithLabelValues(lw.model, lw.enclave, lw.pool, lw.class, reason).Inc()
}

// Flush implements http.Flusher
func (lw *latencyWriter) Flush() {
	if flusher, ok := lw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack implements http.Hijacker when supported by the underlying ResponseWriter.
func (lw *latencyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := lw.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Push implements http.Pusher when supported by the underlying ResponseWriter.
func (lw *latencyWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := lw.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap returns the underlying ResponseWriter (for http.ResponseController)
func (lw *latencyWriter) Unwrap() http.ResponseWriter {
	return lw.ResponseWriter
}

// maxSSEBufferBytes bounds the memory spent buffering one SSE line or one
// event's joined data while scanning for the first token. Token-bearing
// delta chunks are tiny; the only frames that plausibly exceed this are
// control frames that echo the request, like the Responses API's
// response.created — exactly the events detection must ignore. An overflow
// therefore skips the event rather than classifying a truncated frame: at
// worst a pathologically large token event defers detection to the next
// delta, whereas classifying truncated JSON could resurrect the
// first-byte-as-first-token bug this scan exists to fix.
const maxSSEBufferBytes = 64 << 10

// sseTokenDetector incrementally scans an SSE stream, as written to the
// client, for the first event carrying generated output. It understands
// Chat Completions and legacy Completions chunks (choices) and Responses
// API events (typed payloads); everything else — response.created,
// role-only deltas, usage-only chunks, pings, [DONE] — is control traffic.
// State is O(1): at most one partial line and one event's data are
// buffered, both capped at maxSSEBufferBytes.
type sseTokenDetector struct {
	line      []byte // partial line carried across Write boundaries
	data      []byte // joined data: field values of the current event
	hasData   bool
	skipLine  bool // current line overflowed; discard bytes to next newline
	skipEvent bool // current event overflowed; never classify it
	sawError  bool // stream carried an error event before any token
}

// feed scans the next chunk of the stream and reports whether it completed
// an event carrying generated output.
func (d *sseTokenDetector) feed(p []byte) bool {
	token := false
	for len(p) > 0 {
		nl := bytes.IndexByte(p, '\n')
		if nl < 0 {
			d.bufferPartial(p)
			break
		}
		segment := p[:nl]
		p = p[nl+1:]
		if d.skipLine {
			// Tail of an overflowed line: drop it, event stays poisoned.
			d.skipLine = false
			continue
		}
		line := segment
		if len(d.line) > 0 {
			line = append(d.line, segment...)
			d.line = nil
		}
		if len(line) > maxSSEBufferBytes {
			d.skipEvent = true
			continue
		}
		if d.processLine(line) {
			token = true
		}
	}
	return token
}

func (d *sseTokenDetector) bufferPartial(p []byte) {
	if d.skipLine {
		return
	}
	if len(d.line)+len(p) > maxSSEBufferBytes {
		d.line = nil
		d.skipLine = true
		d.skipEvent = true
		return
	}
	d.line = append(d.line, p...)
}

// processLine handles one complete SSE line and reports whether it closed
// an event carrying generated output.
func (d *sseTokenDetector) processLine(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 {
		// Blank line: event boundary, classify what accumulated.
		token := !d.skipEvent && d.hasData && d.classifyEvent(d.data)
		d.data = nil
		d.hasData = false
		d.skipEvent = false
		return token
	}
	value, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		// event:/id:/retry: fields and comment keep-alives carry no
		// output. The Responses API repeats the event type inside the data
		// payload, so data alone suffices to classify.
		return false
	}
	if d.skipEvent {
		return false
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	if len(d.data)+len(value) >= maxSSEBufferBytes {
		d.data = nil
		d.hasData = false
		d.skipEvent = true
		return false
	}
	if d.hasData {
		// Per the SSE spec, multiple data lines in one event join with \n.
		d.data = append(d.data, '\n')
	}
	d.data = append(d.data, value...)
	d.hasData = true
	return false
}

// streamChunk is the union of the SSE payload shapes the router classifies:
// Chat/legacy Completions chunks carry choices; Responses API events carry
// a type plus event-specific fields.
type streamChunk struct {
	Type    string          `json:"type"`
	Delta   json.RawMessage `json:"delta"`
	Text    string          `json:"text"`
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			Refusal          string          `json:"refusal"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
			FunctionCall     json.RawMessage `json:"function_call"`
		} `json:"delta"`
	} `json:"choices"`
}

// classifyEvent reports whether one complete SSE event carries generated
// output, recording error events on the way.
func (d *sseTokenDetector) classifyEvent(data []byte) bool {
	if len(data) == 0 || data[0] != '{' {
		// [DONE] or any other non-object payload is control traffic.
		return false
	}
	var chunk streamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return false
	}
	if jsonPresent(chunk.Error) {
		// In-band engine error on a 200 stream (e.g. vLLM aborts).
		d.sawError = true
		return false
	}
	if chunk.Type != "" { // Responses API event
		switch {
		case chunk.Type == "error" || chunk.Type == "response.failed":
			d.sawError = true
		case strings.HasSuffix(chunk.Type, ".delta"):
			// Text, reasoning, refusal, tool-argument, and audio deltas
			// all carry the payload as a JSON string in the delta field.
			var delta string
			if json.Unmarshal(chunk.Delta, &delta) == nil && delta != "" {
				return true
			}
		case chunk.Type == "response.output_text.done":
			// Safety net for streams that finalize text without deltas.
			return chunk.Text != ""
		}
		return false
	}
	for _, choice := range chunk.Choices {
		if choice.Text != "" || choice.Delta.Content != "" ||
			choice.Delta.ReasoningContent != "" || choice.Delta.Reasoning != "" ||
			choice.Delta.Refusal != "" {
			return true
		}
		if nonEmptyJSONArray(choice.Delta.ToolCalls) || jsonPresent(choice.Delta.FunctionCall) {
			return true
		}
	}
	return false
}

// jsonPresent reports whether a raw JSON field was present and non-null.
func jsonPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func nonEmptyJSONArray(raw json.RawMessage) bool {
	var arr []json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &arr) == nil && len(arr) > 0
}
