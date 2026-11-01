package toolruntime

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
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
	if opts.WebSearch != nil || opts.PIICheck != nil {
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
		"web_search_options": {},
		"pii_check_options": {}
	}`)

	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.WebSearch == nil {
		t.Fatalf("WebSearch must be populated when web_search_options is present")
	}
	if opts.PIICheck == nil {
		t.Fatalf("PIICheck must be populated when pii_check_options is present")
	}
	for _, field := range []string{"web_search_options", "pii_check_options"} {
		if _, leaked := body[field]; leaked {
			t.Fatalf("%s must be stripped from body after extraction", field)
		}
	}
	if got := body["model"]; got != "llama-3" {
		t.Fatalf("non-router fields must be preserved")
	}
}

func TestExtractRouterOptions_ExternalWebAccessMustBeBoolean(t *testing.T) {
	cases := []struct {
		body      string
		wantParam string
	}{
		{`{"web_search_options": {"external_web_access": false}}`, ""},
		{`{"web_search_options": {"external_web_access": true}}`, ""},
		{`{"web_search_options": {"external_web_access": null}}`, ""},
		{`{"tools": [{"type": "function", "name": "f", "external_web_access": "x"}, {"type": "web_search", "external_web_access": false}]}`, ""},
		{`{"web_search_options": {"external_web_access": "false"}}`, "web_search_options.external_web_access"},
		{`{"tools": [{"type": "function", "name": "f"}, {"type": "web_search", "external_web_access": 0}]}`, "tools[1].external_web_access"},
	}
	for _, tc := range cases {
		_, err := ExtractRouterOptions(unmarshal(t, tc.body))
		if tc.wantParam == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.body, err)
			}
			continue
		}
		var apiErr *manager.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || apiErr.Param != tc.wantParam {
			t.Errorf("%s: got %v, want 400 for param %q", tc.body, err, tc.wantParam)
		}
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
	if opts.WebSearch == nil {
		t.Fatalf("WebSearch must be populated when web_search_options is present")
	}
	if opts.PIICheck != nil {
		t.Fatalf("PIICheck must be nil when pii_check_options is absent")
	}
}
