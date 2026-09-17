package toolruntime

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
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
