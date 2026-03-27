package websearch

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/openaiapi"
)

func TestPrepareResponsesPreservesOriginalBody(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4",
		"input":"hello",
		"tools":[
			{"type":"web_search_preview"},
			{"type":"code_interpreter","container":{"type":"auto"}}
		],
		"tool_choice":{"type":"web_search_preview"}
	}`)
	req, handled, err := openaiapi.ParseRequest("/v1/responses", http.Header{}, body)
	if err != nil || !handled {
		t.Fatalf("failed to parse request: handled=%v err=%v", handled, err)
	}

	proxy, err := New().Prepare(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proxy == nil {
		t.Fatal("expected proxy request")
	}
	if string(proxy.Body) != string(body) {
		t.Fatalf("expected original body to be preserved\nwant: %s\ngot:  %s", string(body), string(proxy.Body))
	}
}

func TestPrepareChatPreservesOriginalBody(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4",
		"messages":[{"role":"user","content":"hello"}],
		"web_search_options":{},
		"code_interpreter_options":{"container":{"type":"auto"}}
	}`)
	req, handled, err := openaiapi.ParseRequest("/v1/chat/completions", http.Header{}, body)
	if err != nil || !handled {
		t.Fatalf("failed to parse request: handled=%v err=%v", handled, err)
	}

	proxy, err := New().Prepare(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proxy == nil {
		t.Fatal("expected proxy request")
	}

	var expected map[string]json.RawMessage
	var actual map[string]json.RawMessage
	if err := json.Unmarshal(body, &expected); err != nil {
		t.Fatalf("failed to parse expected body: %v", err)
	}
	if err := json.Unmarshal(proxy.Body, &actual); err != nil {
		t.Fatalf("failed to parse proxy body: %v", err)
	}
	if string(actual["code_interpreter_options"]) != string(expected["code_interpreter_options"]) {
		t.Fatalf("expected code_interpreter_options to be preserved")
	}
	if _, ok := actual["web_search_options"]; !ok {
		t.Fatal("expected web_search_options to be preserved on the first hop")
	}
}
