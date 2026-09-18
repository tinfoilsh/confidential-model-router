package manager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteAPIErrorEmitsOpenAIEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAPIError(rec, ErrModelNotFound.WithMessage(ErrMsgModelNotFound, "gpt-oss-120b"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	inner := body["error"]
	if inner == nil {
		t.Fatal("missing error envelope")
	}
	for _, key := range []string{"message", "type", "param", "code"} {
		if _, ok := inner[key]; !ok {
			t.Errorf("missing field %q", key)
		}
	}
	if len(inner) != 4 {
		t.Errorf("envelope has %d fields, want 4: %v", len(inner), inner)
	}
	if inner["message"] != "The model 'gpt-oss-120b' does not exist or you do not have access to it." {
		t.Errorf("message = %v", inner["message"])
	}
	if inner["type"] != ErrTypeInvalidRequest || inner["code"] != ErrCodeModelNotFound || inner["param"] != "model" {
		t.Errorf("type/code/param = %v/%v/%v", inner["type"], inner["code"], inner["param"])
	}
}

func TestWriteAPIErrorNullsAbsentFields(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAPIError(rec, &ErrServer)

	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	inner := body["error"]
	if v, ok := inner["param"]; !ok || v != nil {
		t.Errorf("param = %v, want null", v)
	}
	if v, ok := inner["code"]; !ok || v != nil {
		t.Errorf("code = %v, want null", v)
	}
}

func TestAPIErrorWithHelpersDoNotMutateBase(t *testing.T) {
	derived := ErrInvalidRequest.WithParam("model").WithMessage("x")
	if ErrInvalidRequest.Param != "" || ErrInvalidRequest.Message != "" {
		t.Fatal("base error was mutated")
	}
	if derived.Param != "model" || derived.Message != "x" || derived.Status != http.StatusBadRequest {
		t.Fatalf("derived = %+v", derived)
	}
}

func TestNormalizeUpstreamError(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		recognized bool
		wantType   string
		wantMsg    string
		wantCode   string
		wantParam  string
	}{
		{
			name:       "vllm nested envelope with python type name",
			status:     http.StatusBadRequest,
			body:       `{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"BadRequestError","param":null,"code":400}}`,
			recognized: true,
			wantType:   ErrTypeInvalidRequest,
			wantMsg:    "This model's maximum context length is 8192 tokens.",
		},
		{
			name:       "vllm flat object shape",
			status:     http.StatusNotFound,
			body:       `{"object":"error","message":"The model x does not exist.","type":"NotFoundError","param":null,"code":404}`,
			recognized: true,
			wantType:   ErrTypeInvalidRequest,
			wantMsg:    "The model x does not exist.",
		},
		{
			name:       "openai types and string codes pass through",
			status:     http.StatusTooManyRequests,
			body:       `{"error":{"message":"slow down","type":"rate_limit_error","param":"messages","code":"slow_down"}}`,
			recognized: true,
			wantType:   ErrTypeRateLimit,
			wantMsg:    "slow down",
			wantCode:   "slow_down",
			wantParam:  "messages",
		},
		{
			name:       "missing type is inferred from status",
			status:     http.StatusInternalServerError,
			body:       `{"error":{"message":"engine crashed"}}`,
			recognized: true,
			wantType:   ErrTypeServer,
			wantMsg:    "engine crashed",
		},
		{
			name:       "non-json body is replaced",
			status:     http.StatusBadGateway,
			body:       `<html>nginx 502</html>`,
			recognized: false,
			wantType:   ErrTypeServer,
			wantMsg:    ErrMsgServerError,
			wantCode:   ErrCodeUpstreamError,
		},
		{
			name:       "json without error object is replaced",
			status:     http.StatusServiceUnavailable,
			body:       `{"detail":"loading"}`,
			recognized: false,
			wantType:   ErrTypeServer,
			wantMsg:    ErrMsgServerError,
			wantCode:   ErrCodeUpstreamError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, recognized := NormalizeUpstreamError(tc.status, []byte(tc.body))
			if recognized != tc.recognized {
				t.Fatalf("recognized = %v, want %v", recognized, tc.recognized)
			}
			if got.Status != tc.status {
				t.Fatalf("status = %d, want %d", got.Status, tc.status)
			}
			if got.Type != tc.wantType || got.Message != tc.wantMsg || got.Code != tc.wantCode || got.Param != tc.wantParam {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
