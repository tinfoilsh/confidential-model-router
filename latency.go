package main

import (
	"bufio"
	"net"
	"net/http"
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

// latencyWriter wraps http.ResponseWriter to observe time-to-first-byte and
// inter-chunk gaps on streaming responses. Only 2xx responses are observed,
// so proxy error bodies don't register as fast first tokens. It deliberately
// does not implement io.ReaderFrom: a delegating ReadFrom would bypass Write
// and drop the per-chunk timing.
type latencyWriter struct {
	http.ResponseWriter
	model   string
	class   string
	status  int
	start   time.Time
	last    time.Time
	nowFunc func() time.Time
}

func newLatencyWriter(w http.ResponseWriter, model, class string) *latencyWriter {
	return &latencyWriter{
		ResponseWriter: w,
		model:          model,
		class:          class,
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
	if lw.status == 0 {
		lw.status = code
	}
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *latencyWriter) Write(b []byte) (int, error) {
	// status 0 means an implicit 200 from a Write without WriteHeader
	if lw.status < 300 {
		now := lw.now()
		if lw.last.IsZero() {
			manager.TTFTSeconds.WithLabelValues(lw.model, lw.class).Observe(now.Sub(lw.start).Seconds())
		} else {
			manager.InterTokenSeconds.WithLabelValues(lw.model, lw.class).Observe(now.Sub(lw.last).Seconds())
		}
		lw.last = now
	}
	return lw.ResponseWriter.Write(b)
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
