package manager

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

// usageWriterKey is the context key for storing the usage metrics writer
type usageWriterKey struct{}

// usageMetricsWriter wraps http.ResponseWriter to capture usage and write trailers
type usageMetricsWriter struct {
	http.ResponseWriter
	usage          *tokencount.Usage
	trailerEnabled bool
	mu             sync.Mutex
}

// SetUsage stores usage data to be written as a header or trailer
func (w *usageMetricsWriter) SetUsage(usage *tokencount.Usage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.usage = usage
}

// EnableTrailer allows writing usage metrics as a trailer
func (w *usageMetricsWriter) EnableTrailer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.trailerEnabled = true
}

// TrailerEnabled reports if trailer writing is enabled
func (w *usageMetricsWriter) TrailerEnabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.trailerEnabled
}

// FormatUsage returns the formatted usage string for the header value
func (w *usageMetricsWriter) FormatUsage() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.usage == nil {
		return ""
	}
	return formatUsage(w.usage)
}

// WriteTrailer writes the usage metrics as an HTTP trailer
// This should be called after the response body has been fully written
func (w *usageMetricsWriter) WriteTrailer() {
	w.mu.Lock()
	if !w.trailerEnabled || w.usage == nil {
		w.mu.Unlock()
		return
	}
	formatted := formatUsage(w.usage)
	w.mu.Unlock()
	// Set the trailer value - Go's http package will send it after the body
	w.Header().Set(UsageMetricsResponseHeader, formatted)
}

// Flush implements http.Flusher
func (w *usageMetricsWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack implements http.Hijacker when supported by the underlying ResponseWriter.
func (w *usageMetricsWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Push implements http.Pusher when supported by the underlying ResponseWriter.
func (w *usageMetricsWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// ReadFrom implements io.ReaderFrom when supported by the underlying ResponseWriter.
func (w *usageMetricsWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

// Unwrap returns the underlying ResponseWriter (for http.ResponseController)
func (w *usageMetricsWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
