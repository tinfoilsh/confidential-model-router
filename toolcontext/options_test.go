package toolcontext

import (
	"encoding/json"
	"testing"
)

func unmarshal(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	return m
}

func TestExtractRouterOptions_PlainBodyHasNoOptions(t *testing.T) {
	body := unmarshal(t, `{
		"model": "llama-3",
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.CodeExecution != nil || opts.WebSearchActive || opts.PIICheckActive {
		t.Fatalf("plain body must yield empty RouterOptions, got %+v", opts)
	}
	if _, ok := body["model"]; !ok {
		t.Fatalf("plain body must be returned untouched")
	}
}

func TestExtractRouterOptions_StripsAllOptionFieldsFromBody(t *testing.T) {
	body := unmarshal(t, `{
		"model": "llama-3",
		"messages": [{"role": "user", "content": "hi"}],
		"code_execution_options": {
			"accessToken": "tok-a",
			"encryptionKey": "key-b",
			"containerAuthToken": "ctr-c"
		},
		"web_search_options": {},
		"pii_check_options": {}
	}`)

	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.CodeExecution == nil {
		t.Fatalf("CodeExecution must be populated when code_execution_options is present")
	}
	if opts.CodeExecution.AccessToken != "tok-a" ||
		opts.CodeExecution.EncryptionKey != "key-b" ||
		opts.CodeExecution.ContainerAuthToken != "ctr-c" {
		t.Fatalf("CodeExecution fields mismatch: %+v", opts.CodeExecution)
	}
	if !opts.WebSearchActive {
		t.Fatalf("WebSearchActive must be true when web_search_options is present")
	}
	if !opts.PIICheckActive {
		t.Fatalf("PIICheckActive must be true when pii_check_options is present")
	}
	for _, field := range []string{"code_execution_options", "web_search_options", "pii_check_options"} {
		if _, leaked := body[field]; leaked {
			t.Fatalf("%s must be stripped from body after extraction", field)
		}
	}
	if got := body["model"]; got != "llama-3" {
		t.Fatalf("non-router fields must be preserved")
	}
}

func TestExtractRouterOptions_CodeExecMissingFieldsAreRejected(t *testing.T) {
	cases := []string{
		`{"code_execution_options": {}}`,
		`{"code_execution_options": {"accessToken": "tok"}}`,
		`{"code_execution_options": {"accessToken": "tok", "encryptionKey": "key"}}`,
		`{"code_execution_options": {"encryptionKey": "key", "containerAuthToken": "ctr"}}`,
	}
	for _, raw := range cases {
		body := unmarshal(t, raw)
		if _, err := ExtractRouterOptions(body); err == nil {
			t.Fatalf("expected error for partial code_execution_options: %s", raw)
		}
	}
}

func TestExtractRouterOptions_NonObjectCodeExecIsRejected(t *testing.T) {
	body := unmarshal(t, `{"code_execution_options": "not-an-object"}`)
	if _, err := ExtractRouterOptions(body); err == nil {
		t.Fatalf("expected error for non-object code_execution_options")
	}
}

func TestExtractRouterOptions_NonStringCredentialFieldsAreRejected(t *testing.T) {
	// Numeric / null values for credential subfields don't satisfy the
	// string assertion, fall through to empty-string check, and produce
	// the same "required" error as a missing field.
	body := unmarshal(t, `{
		"code_execution_options": {
			"accessToken": 42,
			"encryptionKey": "key",
			"containerAuthToken": "ctr"
		}
	}`)
	if _, err := ExtractRouterOptions(body); err == nil {
		t.Fatalf("expected error for non-string accessToken")
	}
}

func TestExtractRouterOptions_OnlySomeOptionsPresent(t *testing.T) {
	body := unmarshal(t, `{
		"model": "x",
		"web_search_options": {}
	}`)
	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.CodeExecution != nil {
		t.Fatalf("CodeExecution must be nil when code_execution_options is absent")
	}
	if !opts.WebSearchActive {
		t.Fatalf("WebSearchActive must be true")
	}
	if opts.PIICheckActive {
		t.Fatalf("PIICheckActive must be false when pii_check_options is absent")
	}
}
