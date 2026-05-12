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
	if opts.CodeExecution != nil || opts.WebSearch != nil || opts.PIICheck != nil {
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
	if opts.WebSearch == nil {
		t.Fatalf("WebSearch must be populated when web_search_options is present")
	}
	if opts.PIICheck == nil {
		t.Fatalf("PIICheck must be populated when pii_check_options is present")
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

func TestExtractRouterOptions_WebSearchInnerPayloadIsPreserved(t *testing.T) {
	// The toolruntime layer parses the inner fields (search_context_size,
	// user_location, filters, advanced knobs) at the MCP boundary. Here
	// we just pin that ExtractRouterOptions lifts the inner map verbatim
	// onto opts.WebSearch.Raw so those parsers find what they expect.
	body := unmarshal(t, `{
		"web_search_options": {
			"search_context_size": "high",
			"user_location": {"approximate": {"country": "us"}},
			"filters": {"allowed_domains": ["example.com"]},
			"max_age_hours": 24
		}
	}`)

	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.WebSearch == nil || opts.WebSearch.Raw == nil {
		t.Fatalf("WebSearch.Raw must carry the inner web_search_options block")
	}
	if got := opts.WebSearch.Raw["search_context_size"]; got != "high" {
		t.Fatalf("search_context_size mismatch: %v", got)
	}
	if _, ok := opts.WebSearch.Raw["filters"]; !ok {
		t.Fatalf("filters sub-block must be preserved")
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
	if opts.WebSearch == nil {
		t.Fatalf("WebSearch must be populated when web_search_options is present")
	}
	if opts.PIICheck != nil {
		t.Fatalf("PIICheck must be nil when pii_check_options is absent")
	}
}
