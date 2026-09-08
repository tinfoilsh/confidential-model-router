package toolruntime

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseHeartbeatInterval is the longest the tool runtime lets a client stream
// sit idle before emitting a comment frame. Tool execution and the
// re-prompt that follows can exceed the idle timeouts of clients and edge
// proxies (Cloudflare gives up after 100s), so the value stays well under
// those while remaining infrequent enough to be negligible on the wire.
const sseHeartbeatInterval = 15 * time.Second

// sseHeartbeatFrame is an SSE comment line. Parsers ignore it by
// specification, so it keeps the connection alive without surfacing an
// event to the consumer.
const sseHeartbeatFrame = ": ping\n\n"

// heartbeatWriter wraps the client ResponseWriter for a tool-runtime
// stream. Between upstream iterations the streamer has nothing to forward
// while MCP tools run and the next model turn warms up, and that silence
// is indistinguishable from a dead connection to anything downstream.
// Once SSE headers have been written, a background goroutine emits a
// comment frame whenever no bytes have gone out for sseHeartbeatInterval.
//
// All writes and flushes are serialised through mu because the streamer
// and the heartbeat goroutine share the underlying ResponseWriter, which
// is not safe for concurrent use.
type heartbeatWriter struct {
	http.ResponseWriter
	flusher http.Flusher

	mu        sync.Mutex
	lastWrite time.Time
	armed     bool
	failed    bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// newHeartbeatWriter returns a writer that starts heartbeating after the
// first successful SSE WriteHeader. Callers must invoke Stop before the
// handler returns so no heartbeat is written to a finished response.
func newHeartbeatWriter(w http.ResponseWriter, flusher http.Flusher) *heartbeatWriter {
	return &heartbeatWriter{
		ResponseWriter: w,
		flusher:        flusher,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
}

func (h *heartbeatWriter) WriteHeader(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ResponseWriter.WriteHeader(status)
	if h.armed || status < 200 || status >= 300 {
		return
	}
	if !strings.HasPrefix(h.Header().Get("Content-Type"), "text/event-stream") {
		return
	}
	h.armed = true
	h.lastWrite = time.Now()
	go h.run()
}

func (h *heartbeatWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.ResponseWriter.Write(p)
	h.lastWrite = time.Now()
	if err != nil {
		h.failed = true
	}
	return n, err
}

func (h *heartbeatWriter) Flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flusher.Flush()
}

// Stop ends the heartbeat goroutine and blocks until it has exited, so a
// caller that defers Stop can be sure nothing touches the ResponseWriter
// after the handler returns.
func (h *heartbeatWriter) Stop() {
	h.stopOnce.Do(func() { close(h.stop) })
	h.mu.Lock()
	armed := h.armed
	h.mu.Unlock()
	if armed {
		<-h.done
	}
}

func (h *heartbeatWriter) run() {
	defer close(h.done)
	ticker := time.NewTicker(sseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case now := <-ticker.C:
			if !h.beat(now) {
				return
			}
		}
	}
}

// beat writes a heartbeat if the stream has been idle for a full interval.
// It reports false once a write has failed, at which point the client is
// gone and further heartbeats are pointless.
func (h *heartbeatWriter) beat(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed {
		return false
	}
	if now.Sub(h.lastWrite) < sseHeartbeatInterval {
		return true
	}
	if _, err := io.WriteString(h.ResponseWriter, sseHeartbeatFrame); err != nil {
		h.failed = true
		return false
	}
	h.flusher.Flush()
	h.lastWrite = now
	return true
}
