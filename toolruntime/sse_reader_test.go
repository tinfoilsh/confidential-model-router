package toolruntime

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReader_DataOnlyFrames(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: {\"a\":1}\n\ndata: [DONE]\n\n"))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("first next err: %v", err)
	}
	if ev.event != "" || ev.data != `{"a":1}` {
		t.Fatalf("first event = %+v, want data-only json", ev)
	}
	ev, err = r.next()
	if err != nil {
		t.Fatalf("second next err: %v", err)
	}
	if ev.data != "[DONE]" {
		t.Fatalf("second event data = %q, want [DONE]", ev.data)
	}
	if _, err := r.next(); err != io.EOF {
		t.Fatalf("third next err = %v, want EOF", err)
	}
}

func TestSSEReader_EventAndData(t *testing.T) {
	input := "event: response.created\ndata: {\"id\":\"r1\"}\n\nevent: response.completed\ndata: {\"id\":\"r1\",\"done\":true}\n\n"
	r := newSSEReader(strings.NewReader(input))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("first next err: %v", err)
	}
	if ev.event != "response.created" || ev.data != `{"id":"r1"}` {
		t.Fatalf("first event = %+v", ev)
	}
	ev, err = r.next()
	if err != nil {
		t.Fatalf("second next err: %v", err)
	}
	if ev.event != "response.completed" || ev.data != `{"id":"r1","done":true}` {
		t.Fatalf("second event = %+v", ev)
	}
}

func TestSSEReader_MultilineData(t *testing.T) {
	input := "data: line1\ndata: line2\n\n"
	r := newSSEReader(strings.NewReader(input))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("next err: %v", err)
	}
	if ev.data != "line1\nline2" {
		t.Fatalf("data = %q, want 'line1\\nline2'", ev.data)
	}
}

func TestSSEReader_IgnoresCommentsAndUnknownFields(t *testing.T) {
	input := ": heartbeat\nid: 5\nretry: 100\ndata: ok\n\n"
	r := newSSEReader(strings.NewReader(input))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("next err: %v", err)
	}
	if ev.data != "ok" {
		t.Fatalf("data = %q, want 'ok'", ev.data)
	}
}

func TestSSEReader_MissingTrailingBlankLineYieldsFinalEvent(t *testing.T) {
	input := "data: hello\n"
	r := newSSEReader(strings.NewReader(input))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("next err: %v", err)
	}
	if ev.data != "hello" {
		t.Fatalf("data = %q, want 'hello'", ev.data)
	}
	if _, err := r.next(); err != io.EOF {
		t.Fatalf("after final event err = %v, want EOF", err)
	}
}

// TestSSEReader_CommentOnlyFramesAreSwallowed pins the HTML5 SSE
// dispatch rule that comment-only frames (`: ping\n\n`) do not produce
// an event. Without this, heartbeats would surface to the caller as
// empty-data frames, forcing every consumer to filter them out
// separately and risking misdispatch on future callers that look at
// event name before checking data presence.
func TestSSEReader_CommentOnlyFramesAreSwallowed(t *testing.T) {
	input := ": ping\n\n: ping\n\ndata: real\n\n"
	r := newSSEReader(strings.NewReader(input))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("next err: %v", err)
	}
	if ev.data != "real" {
		t.Fatalf("expected first returned frame to be the data: frame, got %+v", ev)
	}
	if _, err := r.next(); err != io.EOF {
		t.Fatalf("expected EOF after real frame, got %v", err)
	}
}

// TestSSEReader_EventOnlyFrameIsSwallowed pins that a frame with an
// `event:` header but no `data:` field does not dispatch, matching the
// SSE spec's rule that only frames carrying data trigger a callback.
func TestSSEReader_EventOnlyFrameIsSwallowed(t *testing.T) {
	input := "event: ghost\n\ndata: real\n\n"
	r := newSSEReader(strings.NewReader(input))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("next err: %v", err)
	}
	if ev.event != "" || ev.data != "real" {
		t.Fatalf("expected ghost event to be swallowed, got %+v", ev)
	}
}

func TestSSEReader_ValueLeadingSpaceStripped(t *testing.T) {
	input := "data:no-space\n\ndata: with-space\n\n"
	r := newSSEReader(strings.NewReader(input))
	ev, err := r.next()
	if err != nil {
		t.Fatalf("first next err: %v", err)
	}
	if ev.data != "no-space" {
		t.Fatalf("first data = %q", ev.data)
	}
	ev, err = r.next()
	if err != nil {
		t.Fatalf("second next err: %v", err)
	}
	if ev.data != "with-space" {
		t.Fatalf("second data = %q", ev.data)
	}
}
