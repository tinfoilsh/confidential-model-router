package toolruntime

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/toolprofile"
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

func TestExtractRouterOptions_CodeExecUploadsAbsentMeansNil(t *testing.T) {
	body := unmarshal(t, `{
		"code_execution_options": {
			"accessToken": "tok-a",
			"encryptionKey": "key-b",
			"containerAuthToken": "ctr-c"
		}
	}`)
	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.CodeExecution.Uploads != nil {
		t.Fatalf("Uploads must be nil when uploads field is absent, got %+v", *opts.CodeExecution.Uploads)
	}
}

func TestExtractRouterOptions_CodeExecUploadsEmptyArrayPreserved(t *testing.T) {
	// Empty array (not absent) is the signal to clear /user-uploads;
	// must round-trip as non-nil pointer to a zero-length slice.
	body := unmarshal(t, `{
		"code_execution_options": {
			"accessToken": "tok-a",
			"encryptionKey": "key-b",
			"containerAuthToken": "ctr-c",
			"uploads": []
		}
	}`)
	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.CodeExecution.Uploads == nil {
		t.Fatalf("Uploads must be non-nil when uploads:[] is sent")
	}
	if len(*opts.CodeExecution.Uploads) != 0 {
		t.Fatalf("Uploads must be empty, got %+v", *opts.CodeExecution.Uploads)
	}
}

func TestExtractRouterOptions_CodeExecUploadsParsesEntries(t *testing.T) {
	body := unmarshal(t, `{
		"code_execution_options": {
			"accessToken": "tok-a",
			"encryptionKey": "key-b",
			"containerAuthToken": "ctr-c",
			"uploads": [
				{"fileAccessToken": "fat-1", "filename": "report.pdf", "sha256": "abc"},
				{"fileAccessToken": "fat-2", "filename": "data.csv",   "sha256": "def"}
			]
		}
	}`)
	opts, err := ExtractRouterOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.CodeExecution.Uploads == nil || len(*opts.CodeExecution.Uploads) != 2 {
		t.Fatalf("expected 2 uploads, got %+v", opts.CodeExecution.Uploads)
	}
	got := (*opts.CodeExecution.Uploads)[0]
	if got.FileAccessToken != "fat-1" || got.Filename != "report.pdf" || got.Sha256 != "abc" {
		t.Fatalf("upload[0] mismatch: %+v", got)
	}
	got = (*opts.CodeExecution.Uploads)[1]
	if got.FileAccessToken != "fat-2" || got.Filename != "data.csv" || got.Sha256 != "def" {
		t.Fatalf("upload[1] mismatch: %+v", got)
	}
}

func TestExtractRouterOptions_CodeExecUploadsRejectsBadShape(t *testing.T) {
	cases := map[string]string{
		"not-an-array":          `{"code_execution_options":{"accessToken":"a","encryptionKey":"b","containerAuthToken":"c","uploads":"oops"}}`,
		"entry-not-object":      `{"code_execution_options":{"accessToken":"a","encryptionKey":"b","containerAuthToken":"c","uploads":["oops"]}}`,
		"entry-missing-fat":     `{"code_execution_options":{"accessToken":"a","encryptionKey":"b","containerAuthToken":"c","uploads":[{"filename":"x","sha256":"y"}]}}`,
		"entry-missing-name":    `{"code_execution_options":{"accessToken":"a","encryptionKey":"b","containerAuthToken":"c","uploads":[{"fileAccessToken":"x","sha256":"y"}]}}`,
		"entry-missing-sha":     `{"code_execution_options":{"accessToken":"a","encryptionKey":"b","containerAuthToken":"c","uploads":[{"fileAccessToken":"x","filename":"y"}]}}`,
		"entry-non-string-name": `{"code_execution_options":{"accessToken":"a","encryptionKey":"b","containerAuthToken":"c","uploads":[{"fileAccessToken":"x","filename":42,"sha256":"y"}]}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			body := unmarshal(t, raw)
			if _, err := ExtractRouterOptions(body); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
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

// codeExecMeta returns the inner tinfoil_code_exec map after running
// attachRouterOptionsMeta on a freshly built registry — the boundary
// the MCP server actually sees.
func codeExecMeta(t *testing.T, opts *RouterOptions) map[string]any {
	t.Helper()
	r := &sessionRegistry{metaByProfile: map[string]mcp.Meta{}}
	attachRouterOptionsMeta(r, opts)
	meta := r.metaByProfile[toolprofile.CodeExecution.Name]
	if meta == nil {
		t.Fatalf("expected meta for %q, got nil", toolprofile.CodeExecution.Name)
	}
	block, ok := meta[toolprofile.CodeExecutionMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("meta block has wrong type: %T", meta[toolprofile.CodeExecutionMetaKey])
	}
	return block
}

func TestAttachRouterOptionsMeta_NoUploadsKeyWhenAbsent(t *testing.T) {
	block := codeExecMeta(t, &RouterOptions{
		CodeExecution: &CodeExecutionOptions{
			AccessToken: "a", EncryptionKey: "b", ContainerAuthToken: "c",
		},
	})
	if _, present := block["uploads"]; present {
		t.Fatalf("uploads key must be absent when ce.Uploads is nil; got %+v", block)
	}
}

func TestAttachRouterOptionsMeta_UploadsKeyPresentWhenEmpty(t *testing.T) {
	empty := []UploadedFile{}
	block := codeExecMeta(t, &RouterOptions{
		CodeExecution: &CodeExecutionOptions{
			AccessToken: "a", EncryptionKey: "b", ContainerAuthToken: "c",
			Uploads: &empty,
		},
	})
	got, ok := block["uploads"].([]any)
	if !ok {
		t.Fatalf("uploads must be []any, got %T", block["uploads"])
	}
	if len(got) != 0 {
		t.Fatalf("uploads must be length 0, got %+v", got)
	}
}

func TestAttachRouterOptionsMeta_UploadsForwardedVerbatim(t *testing.T) {
	files := []UploadedFile{
		{FileAccessToken: "fat-1", Filename: "report.pdf", Sha256: "abc"},
		{FileAccessToken: "fat-2", Filename: "data.csv", Sha256: "def"},
	}
	block := codeExecMeta(t, &RouterOptions{
		CodeExecution: &CodeExecutionOptions{
			AccessToken: "a", EncryptionKey: "b", ContainerAuthToken: "c",
			Uploads: &files,
		},
	})
	got, ok := block["uploads"].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("uploads not propagated: %+v", block["uploads"])
	}
	first, _ := got[0].(map[string]any)
	if first["fileAccessToken"] != "fat-1" || first["filename"] != "report.pdf" || first["sha256"] != "abc" {
		t.Fatalf("uploads[0] mismatch: %+v", first)
	}
	second, _ := got[1].(map[string]any)
	if second["fileAccessToken"] != "fat-2" || second["filename"] != "data.csv" || second["sha256"] != "def" {
		t.Fatalf("uploads[1] mismatch: %+v", second)
	}
}
