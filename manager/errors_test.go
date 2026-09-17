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
