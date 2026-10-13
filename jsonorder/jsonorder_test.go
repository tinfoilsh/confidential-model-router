package jsonorder

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

const (
	clientSchema = `{"type":"object","properties":{"reasoning":{"type":"string"},"answer":{"type":"string","enum":["b","a"]}},"required":["reasoning","answer"]}`
	clientTool   = `{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}}}`
)

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestMarshalKeepsClientSchemaOrder(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + clientSchema + `}},` +
		`"tools":[` + clientTool + `]}`
	body := decode(t, raw)
	body["stream"] = true

	got, err := Capture([]byte(raw), decode(t, raw)).Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"messages":[{"content":"hi","role":"user"}],"model":"m",` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + clientSchema + `}},` +
		`"stream":true,"tools":[` + clientTool + `]}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// The tool loop clones the body through encoding/json and appends its own
// tools, so the client's fields must be found by content, not by identity or
// position.
func TestMarshalRestoresClonedBodyWithAppendedTools(t *testing.T) {
	raw := `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + clientSchema + `}},"tools":[` + clientTool + `]}`
	original := Capture([]byte(raw), decode(t, raw))
	cloned, err := json.Marshal(decode(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	body := decode(t, string(cloned))
	body["tools"] = append(body["tools"].([]any), map[string]any{"type": "function", "function": map[string]any{"name": "search"}})

	got, err := FromContext(NewContext(context.Background(), original)).Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + clientSchema + `}},` +
		`"tools":[` + clientTool + `,{"function":{"name":"search"},"type":"function"}]}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMarshalEncodesFieldsTheRouterChanged(t *testing.T) {
	raw := `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + clientSchema + `}}}`
	body := decode(t, raw)
	body["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"] = "renamed"

	got, err := Capture([]byte(raw), decode(t, raw)).Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCaptureMatchesEscapedNames(t *testing.T) {
	raw := `{"model":"m","response\u005fformat":{"type":"json_schema","json_schema":{"name":"answer","schema":` + clientSchema + `}}}`
	got, err := Capture([]byte(raw), decode(t, raw)).Marshal(decode(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + clientSchema + `}}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// encoding/json keeps the last of duplicate names while other parsers may
// not, so a body with duplicates must not have its bytes forwarded.
func TestCaptureRejectsDuplicateNames(t *testing.T) {
	raw := `{"model":"m","response_format":{"type":"json_schema","type":"json_object"}}`
	if original := Capture([]byte(raw), decode(t, raw)); original != nil {
		t.Fatalf("captured %v from a body with duplicate names", original.raw)
	}
}
