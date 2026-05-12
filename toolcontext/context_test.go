package toolcontext

import (
	"encoding/json"
	"testing"
)

// unmarshal is a small helper so test fixtures read as JSON rather
// than nested map[string]any literals.
func unmarshal(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	return m
}

func TestExtractTinfoilCtx_PlainBodyPassesThrough(t *testing.T) {
	body := unmarshal(t, `{
		"model": "llama-3",
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	ctx, out, wrapped, err := ExtractTinfoilCtx(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wrapped {
		t.Fatalf("plain body must not be marked as wrapped")
	}
	if ctx != nil {
		t.Fatalf("plain body must yield nil ctx, got %+v", ctx)
	}
	if _, ok := out["model"]; !ok {
		t.Fatalf("plain body must be returned unchanged")
	}
}

func TestExtractTinfoilCtx_WrappedBodyUnwraps(t *testing.T) {
	body := unmarshal(t, `{
		"tinfoil_ctx": {
			"accessToken": "tok-a",
			"encryptionKey": "key-b",
			"containerAuthToken": "ctr-c"
		},
		"payload": {
			"model": "llama-3",
			"messages": [{"role": "user", "content": "hi"}],
			"code_execution_options": {}
		}
	}`)

	ctx, out, wrapped, err := ExtractTinfoilCtx(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wrapped {
		t.Fatalf("wrapped body must be marked as wrapped")
	}
	if ctx == nil {
		t.Fatalf("wrapped body must yield non-nil ctx")
	}
	if ctx.AccessToken != "tok-a" || ctx.EncryptionKey != "key-b" || ctx.ContainerAuthToken != "ctr-c" {
		t.Fatalf("ctx fields mismatch: %+v", ctx)
	}
	if _, leaked := out["tinfoil_ctx"]; leaked {
		t.Fatalf("unwrapped body must not contain tinfoil_ctx")
	}
	if _, leaked := out["payload"]; leaked {
		t.Fatalf("unwrapped body must not contain payload key (it IS the payload)")
	}
	if got := out["model"]; got != "llama-3" {
		t.Fatalf("unwrapped body missing payload contents, got %+v", out)
	}
	if _, ok := out["code_execution_options"]; !ok {
		t.Fatalf("unwrapped body must preserve payload fields")
	}
}

func TestExtractTinfoilCtx_MissingFieldsAreEmptyStrings(t *testing.T) {
	// Tolerate partial ctx — the toolruntime layer decides which
	// headers to forward based on non-emptiness, and the MCP server
	// is the authority on what is required.
	body := unmarshal(t, `{
		"tinfoil_ctx": {"accessToken": "tok-a"},
		"payload": {"model": "llama-3"}
	}`)

	ctx, _, wrapped, err := ExtractTinfoilCtx(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wrapped {
		t.Fatalf("wrapped body must be marked as wrapped")
	}
	if ctx.AccessToken != "tok-a" {
		t.Fatalf("AccessToken must be preserved")
	}
	if ctx.EncryptionKey != "" || ctx.ContainerAuthToken != "" {
		t.Fatalf("missing ctx fields must default to empty string, got %+v", ctx)
	}
}

func TestExtractTinfoilCtx_NonObjectCtxIsRejected(t *testing.T) {
	body := unmarshal(t, `{
		"tinfoil_ctx": "not-an-object",
		"payload": {"model": "x"}
	}`)

	if _, _, _, err := ExtractTinfoilCtx(body); err == nil {
		t.Fatalf("expected error for non-object tinfoil_ctx")
	}
}

func TestExtractTinfoilCtx_MissingPayloadIsRejected(t *testing.T) {
	body := unmarshal(t, `{
		"tinfoil_ctx": {"accessToken": "tok"}
	}`)

	if _, _, _, err := ExtractTinfoilCtx(body); err == nil {
		t.Fatalf("expected error when payload is missing")
	}
}

func TestExtractTinfoilCtx_NonObjectPayloadIsRejected(t *testing.T) {
	body := unmarshal(t, `{
		"tinfoil_ctx": {"accessToken": "tok"},
		"payload": "not-an-object"
	}`)

	if _, _, _, err := ExtractTinfoilCtx(body); err == nil {
		t.Fatalf("expected error for non-object payload")
	}
}

func TestExtractTinfoilCtx_WrongCtxFieldTypesAreDroppedSilently(t *testing.T) {
	// Numeric / null fields don't satisfy the string assertion in
	// stringField and are treated the same as missing — they don't
	// surface as a validation error here. The MCP server will reject
	// requests it cannot serve.
	body := unmarshal(t, `{
		"tinfoil_ctx": {"accessToken": 42, "encryptionKey": null, "containerAuthToken": "ok"},
		"payload": {"model": "x"}
	}`)

	ctx, _, _, err := ExtractTinfoilCtx(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx.AccessToken != "" || ctx.EncryptionKey != "" || ctx.ContainerAuthToken != "ok" {
		t.Fatalf("non-string fields must coerce to empty, got %+v", ctx)
	}
}
