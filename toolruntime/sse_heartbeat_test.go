package toolruntime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newArmedHeartbeat(t *testing.T) (*heartbeatWriter, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	h := newHeartbeatWriter(rec, rec)
	h.Header().Set("Content-Type", "text/event-stream")
	h.WriteHeader(http.StatusOK)
	t.Cleanup(h.Stop)
	return h, rec
}

func TestHeartbeatWritesCommentAfterIdleInterval(t *testing.T) {
	h, rec := newArmedHeartbeat(t)

	now := time.Now()
	if _, ok := h.beat(now.Add(sseHeartbeatInterval / 2)); !ok {
		t.Fatal("beat() reported failure on a healthy writer")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("heartbeat fired before the idle interval elapsed: %q", rec.Body.String())
	}

	if _, ok := h.beat(now.Add(sseHeartbeatInterval)); !ok {
		t.Fatal("beat() reported failure on a healthy writer")
	}
	if got := rec.Body.String(); got != sseHeartbeatFrame {
		t.Fatalf("heartbeat frame = %q, want %q", got, sseHeartbeatFrame)
	}
}

// When a write lands partway through the idle period, the next check must
// be scheduled for the remainder of that period rather than a full interval
// later, otherwise a silence that starts right after a check goes unnoticed
// for almost two intervals.
func TestHeartbeatReschedulesFromLastWrite(t *testing.T) {
	h, _ := newArmedHeartbeat(t)

	h.mu.Lock()
	h.lastWrite = time.Now()
	h.mu.Unlock()

	elapsed := sseHeartbeatInterval / 3
	wait, ok := h.beat(h.lastWrite.Add(elapsed))
	if !ok {
		t.Fatal("beat() reported failure on a healthy writer")
	}
	if want := sseHeartbeatInterval - elapsed; wait != want {
		t.Fatalf("beat() wait = %v, want remainder of idle period %v", wait, want)
	}

	wait, ok = h.beat(h.lastWrite.Add(sseHeartbeatInterval))
	if !ok {
		t.Fatal("beat() reported failure on a healthy writer")
	}
	if wait != sseHeartbeatInterval {
		t.Fatalf("beat() wait after emitting = %v, want full interval %v", wait, sseHeartbeatInterval)
	}
}

func TestHeartbeatIsSuppressedByRecentWrites(t *testing.T) {
	h, rec := newArmedHeartbeat(t)

	frame := "data: {\"choices\":[]}\n\n"
	if _, err := h.Write([]byte(frame)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, ok := h.beat(time.Now().Add(sseHeartbeatInterval / 2)); !ok {
		t.Fatal("beat() reported failure on a healthy writer")
	}
	if got := rec.Body.String(); got != frame {
		t.Fatalf("heartbeat interleaved with a fresh write: %q", got)
	}
}

func TestHeartbeatOnlyArmsForSSEResponses(t *testing.T) {
	rec := httptest.NewRecorder()
	h := newHeartbeatWriter(rec, rec)
	h.Header().Set("Content-Type", "application/json")
	h.WriteHeader(http.StatusOK)
	defer h.Stop()

	if h.armed {
		t.Fatal("heartbeat armed for a non-SSE response")
	}
}

func TestHeartbeatStopsAfterWriteFailure(t *testing.T) {
	w := &failingFlushWriter{}
	h := newHeartbeatWriter(w, w)
	h.Header().Set("Content-Type", "text/event-stream")
	h.WriteHeader(http.StatusOK)
	defer h.Stop()

	if _, ok := h.beat(time.Now().Add(sseHeartbeatInterval)); ok {
		t.Fatal("beat() kept running after the client write failed")
	}
}

// A heartbeat that discovers the client is gone must surface on the
// streamer's next Write so its writeErr latch trips and the tool loop
// stops, even if the underlying writer would otherwise accept bytes again.
func TestHeartbeatFailurePropagatesToLaterWrites(t *testing.T) {
	w := &failOnceWriter{ResponseRecorder: httptest.NewRecorder()}
	h := newHeartbeatWriter(w, w)
	h.Header().Set("Content-Type", "text/event-stream")
	h.WriteHeader(http.StatusOK)
	defer h.Stop()

	if _, ok := h.beat(time.Now().Add(sseHeartbeatInterval)); ok {
		t.Fatal("beat() did not report the failed heartbeat write")
	}
	if _, err := h.Write([]byte("data: {}\n\n")); err == nil {
		t.Fatal("Write() succeeded after the heartbeat detected a dead client")
	}
}

type failOnceWriter struct {
	*httptest.ResponseRecorder
	failed bool
}

func (w *failOnceWriter) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

// Comment frames must be invisible to the router's own SSE reader so a
// heartbeat relayed by an intermediary never surfaces as an event.
func TestHeartbeatFrameIsIgnoredBySSEReader(t *testing.T) {
	input := sseHeartbeatFrame + "data: {\"ok\":true}\n\n"
	reader := newSSEReader(strings.NewReader(input))
	frame, err := reader.next()
	if err != nil {
		t.Fatalf("next() error = %v", err)
	}
	if frame.data != "{\"ok\":true}" {
		t.Fatalf("frame.data = %q, want the data frame following the heartbeat", frame.data)
	}
}
