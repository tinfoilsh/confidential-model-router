package toolruntime

import (
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
	if !h.beat(now.Add(sseHeartbeatInterval / 2)) {
		t.Fatal("beat() reported failure on a healthy writer")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("heartbeat fired before the idle interval elapsed: %q", rec.Body.String())
	}

	if !h.beat(now.Add(sseHeartbeatInterval)) {
		t.Fatal("beat() reported failure on a healthy writer")
	}
	if got := rec.Body.String(); got != sseHeartbeatFrame {
		t.Fatalf("heartbeat frame = %q, want %q", got, sseHeartbeatFrame)
	}
}

func TestHeartbeatIsSuppressedByRecentWrites(t *testing.T) {
	h, rec := newArmedHeartbeat(t)

	frame := "data: {\"choices\":[]}\n\n"
	if _, err := h.Write([]byte(frame)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !h.beat(time.Now().Add(sseHeartbeatInterval / 2)) {
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

	if h.beat(time.Now().Add(sseHeartbeatInterval)) {
		t.Fatal("beat() kept running after the client write failed")
	}
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
