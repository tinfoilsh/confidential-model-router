package toolruntime

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/toolruntime/citations"
)

// assertUpstreamErrorEnvelope checks that a router-synthesized stream
// failure carries the generic upstream error in OpenAI's envelope with a
// 502 status, so protocol-level detail never reaches the client.
func assertUpstreamErrorEnvelope(t *testing.T, upErr *upstreamError) {
	t.Helper()
	if upErr.statusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", upErr.statusCode)
	}
	var envelope manager.ErrorEnvelope
	if err := json.Unmarshal(upErr.body, &envelope); err != nil {
		t.Fatalf("body is not an error envelope: %s", upErr.body)
	}
	if envelope.Error.Type != manager.ErrTypeServer || envelope.Error.Code == nil || *envelope.Error.Code != manager.ErrCodeUpstreamError {
		t.Fatalf("envelope = %+v", envelope.Error)
	}
	if envelope.Error.Message != manager.ErrMsgServerError {
		t.Fatalf("message = %q", envelope.Error.Message)
	}
}

func TestUpstreamErrorPayloadNormalizesBackendErrors(t *testing.T) {
	payload := upstreamErrorPayload(&upstreamError{
		statusCode: http.StatusBadRequest,
		body:       []byte(`{"object":"error","message":"context length exceeded","type":"BadRequestError","code":400}`),
	})
	if payload["message"] != "context length exceeded" || payload["type"] != manager.ErrTypeInvalidRequest {
		t.Fatalf("payload = %v", payload)
	}
	for _, key := range []string{"message", "type", "param", "code"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload missing %q: %v", key, payload)
		}
	}
}

func TestUpstreamErrorPayloadHidesRawBodies(t *testing.T) {
	payload := upstreamErrorPayload(&upstreamError{
		statusCode: http.StatusBadGateway,
		body:       []byte("upstream connect error or disconnect/reset before headers"),
	})
	if payload["message"] != manager.ErrMsgServerError || payload["code"] != manager.ErrCodeUpstreamError {
		t.Fatalf("raw body leaked: %v", payload)
	}
}

// TestStreamAbortedErrorMarksPostHeaderFailures pins that a write failure
// after SSE headers are on the wire is reported as StreamAbortedError, so
// the caller knows not to write a second HTTP response, and that
// writeUpstreamError leaves such errors untouched.
func TestStreamAbortedErrorMarksPostHeaderFailures(t *testing.T) {
	w := &failingFlushWriter{}
	streamer := &chatStreamer{
		streamBase: streamBase{
			w:              w,
			flusher:        w,
			citations:      &citations.State{NextIndex: 1},
			toolCalls:      &toolCallLog{},
			usageTotals:    &usageAccumulator{},
			headersWritten: true,
			model:          "gpt-oss-120b",
		},
		id:      "cmpl",
		created: 1,
	}
	streamer.emitter = citations.NewEmitter(streamer.citations)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	err := streamer.terminateWithError(req, nil, "gpt-oss-120b", &upstreamError{statusCode: http.StatusBadGateway})
	var aborted *StreamAbortedError
	if !errors.As(err, &aborted) {
		t.Fatalf("expected StreamAbortedError, got %T: %v", err, err)
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("aborted error must wrap the write failure, got %v", err)
	}

	rec := httptest.NewRecorder()
	if got := writeUpstreamError(rec, err); got != err {
		t.Fatalf("writeUpstreamError should return the aborted error unchanged, got %v", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("writeUpstreamError wrote a body after stream abort: %s", rec.Body.String())
	}
}

// Before headers are written, a terminal error must not be marked aborted:
// the caller still owns the response and should render a JSON error.
func TestStreamAbortedNotAppliedBeforeHeaders(t *testing.T) {
	streamer := &chatStreamer{streamBase: streamBase{}}
	upErr := &upstreamError{statusCode: http.StatusBadGateway, body: []byte(`{"error":{"message":"x"}}`)}
	err := streamer.terminateWithError(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), nil, "m", upErr)
	if err != upErr {
		t.Fatalf("pre-header error should pass through, got %v", err)
	}
}
