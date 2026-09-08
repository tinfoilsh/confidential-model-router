package toolruntime

import (
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
	failedErr error

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

// Write forwards to the client. Once any write has failed, including a
// heartbeat, every later write fails immediately so the streamer's own
// write-error latch trips on its next emit and the tool loop stops
// spending upstream tokens on a caller that has gone away.
func (h *heartbeatWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed {
		return 0, h.failedErr
	}
	n, err := h.ResponseWriter.Write(p)
	h.lastWrite = time.Now()
	if err != nil {
		h.fail(err)
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

// run wakes exactly when the current idle period reaches
// sseHeartbeatInterval. A fixed-cadence ticker would be wrong here: its
// phase is set when the stream arms, but writes land at arbitrary offsets,
// so a silence that begins just after a tick would not be caught until the
// following one, stretching the worst case to nearly two intervals.
func (h *heartbeatWriter) run() {
	defer close(h.done)
	timer := time.NewTimer(sseHeartbeatInterval)
	defer timer.Stop()
	for {
		select {
		case <-h.stop:
			return
		case now := <-timer.C:
			wait, ok := h.beat(now)
			if !ok {
				return
			}
			timer.Reset(wait)
		}
	}
}

// beat writes a heartbeat if the stream has been idle for a full interval
// and returns how long to wait before checking again: the remainder of the
// current idle period if a write arrived in the meantime, otherwise a full
// interval. It reports false once a write has failed, at which point the
// client is gone and further heartbeats are pointless.
func (h *heartbeatWriter) beat(now time.Time) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed {
		return 0, false
	}
	if idle := now.Sub(h.lastWrite); idle < sseHeartbeatInterval {
		return sseHeartbeatInterval - idle, true
	}
	if _, err := h.ResponseWriter.Write([]byte(sseHeartbeatFrame)); err != nil {
		h.fail(err)
		return 0, false
	}
	h.flusher.Flush()
	h.lastWrite = now
	return sseHeartbeatInterval, true
}

// fail records the first write error; callers hold mu.
func (h *heartbeatWriter) fail(err error) {
	if h.failed {
		return
	}
	h.failed = true
	h.failedErr = err
}
