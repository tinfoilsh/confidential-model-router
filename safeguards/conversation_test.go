package safeguards

import (
	"encoding/json"
	"reflect"
	"testing"
)

func parse(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestRequestMessages_Chat(t *testing.T) {
	body := parse(t, `{"messages":[
		{"role":"system","content":"be helpful"},
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:..."}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},
		{"role":"tool","tool_call_id":"c1","content":"42"},
		{"role":"user","content":"thanks"}
	]}`)
	got := RequestMessages("/v1/chat/completions", body)
	want := []Message{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "look\n[image_url]"},
		{Role: "assistant", Content: ""},
		{Role: "user", Content: "thanks"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestRequestMessages_Responses(t *testing.T) {
	body := parse(t, `{"instructions":"be terse","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:..."}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},
		{"type":"function_call","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"x","output":"y"},
		{"role":"user","content":"next"}
	]}`)
	got := RequestMessages("/v1/responses", body)
	want := []Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "hi\n[input_image]"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "next"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	got = RequestMessages("/v1/responses", parse(t, `{"input":"just a string"}`))
	if !reflect.DeepEqual(got, []Message{{Role: "user", Content: "just a string"}}) {
		t.Fatalf("string input: got %+v", got)
	}
}

func TestRequestMessages_UnknownPath(t *testing.T) {
	if got := RequestMessages("/v1/embeddings", parse(t, `{"input":"x"}`)); got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestResponsesOutputText(t *testing.T) {
	body := parse(t, `{"output":[
		{"type":"reasoning","summary":[]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"},{"type":"output_text","text":"second"}]},
		{"type":"function_call","name":"f"},
		{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}
	]}`)
	if got := ResponsesOutputText(body["output"]); got != "first\nsecond\nno" {
		t.Fatalf("got %q", got)
	}
}
