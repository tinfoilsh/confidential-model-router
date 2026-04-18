package toolruntime

import (
	"encoding/json"
	"net/url"
	"reflect"
	"testing"
)

func TestStripModelControlTokens_CleanInputUnchanged(t *testing.T) {
	cases := []string{
		"",
		"https://example.com",
		"plain string with no tokens",
		"string with a lone < and | characters",
		`{"json":"yes","url":"https://a.test/path"}`,
	}
	for _, in := range cases {
		if got := stripModelControlTokens(in); got != in {
			t.Errorf("clean input mutated: in=%q got=%q", in, got)
		}
	}
}

func TestStripModelControlTokens_GemmaURLLeak(t *testing.T) {
	// The exact pattern observed in /tmp/ux-probe-gemma-post/
	// streaming runs, where fetch-tool URL args arrived wrapped by the
	// gemma chat-template's <|"|...<|"|> markers.
	in := `<|"|https://local.test/cats/gazette<|"|>`
	want := `https://local.test/cats/gazette`
	if got := stripModelControlTokens(in); got != want {
		t.Errorf("strip leaked-URL failed: in=%q got=%q want=%q", in, got, want)
	}
}

func TestStripModelControlTokens_KimiToolCallTokens(t *testing.T) {
	in := `<|tool_call_argument_begin|>{"a":1}<|tool_call_end|>`
	want := `{"a":1}`
	if got := stripModelControlTokens(in); got != want {
		t.Errorf("strip kimi tokens failed: got=%q want=%q", got, want)
	}
}

func TestStripModelControlTokens_Idempotent(t *testing.T) {
	in := `<|"|https://x<|"|>`
	once := stripModelControlTokens(in)
	twice := stripModelControlTokens(once)
	if once != twice {
		t.Errorf("stripModelControlTokens is not idempotent: once=%q twice=%q", once, twice)
	}
}

func TestStripModelControlTokens_LegitimatePipesSurvive(t *testing.T) {
	in := "https://example.com/?x=|y|&z=a"
	if got := stripModelControlTokens(in); got != in {
		t.Errorf("legit pipes got stripped: in=%q got=%q", in, got)
	}
}

func TestStripModelControlTokens_LongSequenceIsNotMatched(t *testing.T) {
	// Longer than 64 runes inside the <| ... |> -> regex should NOT
	// match, so the string is returned unchanged. Guards against a
	// pathological regex DoS across adjacent tokens.
	inner := ""
	for i := 0; i < 80; i++ {
		inner += "x"
	}
	in := "<|" + inner + "|>"
	if got := stripModelControlTokens(in); got != in {
		t.Errorf("overlong token was eaten: got=%q", got)
	}
}

func TestDeepStripURLTokens_AllowlistedField(t *testing.T) {
	args := map[string]any{
		"url":         `<|"|https://local.test/a<|"|>`,
		"max_results": float64(5),
	}
	if !deepStripURLTokens(args) {
		t.Fatalf("deepStripURLTokens reported no change")
	}
	if got := args["url"]; got != "https://local.test/a" {
		t.Errorf("url field not cleaned: %q", got)
	}
	if got := args["max_results"]; got != float64(5) {
		t.Errorf("non-string field mutated: %v", got)
	}
}

func TestDeepStripURLTokens_AllowlistedIsCaseInsensitive(t *testing.T) {
	args := map[string]any{"URL": `<|"|https://x<|"|>`}
	if !deepStripURLTokens(args) {
		t.Fatalf("case-insensitive allowlist match failed")
	}
	if got := args["URL"]; got != "https://x" {
		t.Errorf("uppercase url key not cleaned: %q", got)
	}
}

func TestDeepStripURLTokens_URLsArray(t *testing.T) {
	args := map[string]any{
		"urls": []any{
			`<|"|https://a<|"|>`,
			"https://b",
			`<|tool_call_argument_begin|>https://c<|tool_call_end|>`,
		},
	}
	if !deepStripURLTokens(args) {
		t.Fatalf("array-valued url field not cleaned")
	}
	got, _ := args["urls"].([]any)
	want := []any{"https://a", "https://b", "https://c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("urls array mismatch: got=%v want=%v", got, want)
	}
}

func TestDeepStripURLTokens_NestedObject(t *testing.T) {
	args := map[string]any{
		"page": map[string]any{
			"href": `<|"|https://deep.test/x<|"|>`,
			"id":   float64(3),
		},
	}
	if !deepStripURLTokens(args) {
		t.Fatalf("nested href not cleaned")
	}
	page, _ := args["page"].(map[string]any)
	if got := page["href"]; got != "https://deep.test/x" {
		t.Errorf("nested href: got=%q", got)
	}
}

func TestDeepStripURLTokens_NonURLFieldLeftAlone(t *testing.T) {
	// A string field that is NOT in the allowlist and does NOT look like
	// a leaked URL must be left alone, even if it happens to contain
	// <|...|> characters (which could conceivably come from legitimate
	// user-provided prompt content).
	args := map[string]any{
		"query": "find <|maybe|> a restaurant",
	}
	if deepStripURLTokens(args) {
		t.Fatalf("non-URL field was stripped unexpectedly")
	}
	if got := args["query"]; got != "find <|maybe|> a restaurant" {
		t.Errorf("query field mutated: %q", got)
	}
}

func TestDeepStripURLTokens_LeakedURLInUnknownField(t *testing.T) {
	// A field not in the allowlist but whose value clearly decodes to a
	// URL once tokens are stripped SHOULD be cleaned, because that is
	// exactly the fetch-variant shape we observed.
	args := map[string]any{
		"target": `<|"|https://local.test/almanac<|"|>`,
	}
	if !deepStripURLTokens(args) {
		t.Fatalf("unknown field with leaked URL was not cleaned")
	}
	if got := args["target"]; got != "https://local.test/almanac" {
		t.Errorf("target: got=%q", got)
	}
}

func TestDeepStripURLTokens_CleanInputUnchanged(t *testing.T) {
	before := map[string]any{
		"query": "cats",
		"urls":  []any{"https://a", "https://b"},
		"nested": map[string]any{
			"href": "https://c",
		},
	}
	snapshot := deepCopyJSON(t, before)
	if deepStripURLTokens(before) {
		t.Fatalf("clean input reported as changed")
	}
	if !reflect.DeepEqual(before, snapshot) {
		t.Errorf("clean input mutated: before=%v after=%v", snapshot, before)
	}
}

func TestSanitizeToolCallArguments_ReturnValue(t *testing.T) {
	// Reports false and does not mutate when no repair fires.
	clean := map[string]any{"query": "cats", "max_results": float64(5)}
	if sanitizeToolCallArguments("search", clean) {
		t.Errorf("clean args reported as changed")
	}

	// Reports true when the URL field is cleaned.
	dirty := map[string]any{"url": `<|"|https://x<|"|>`}
	if !sanitizeToolCallArguments("fetch", dirty) {
		t.Errorf("dirty args reported as clean")
	}
	if got := dirty["url"]; got != "https://x" {
		t.Errorf("dirty args not actually repaired: %q", got)
	}
}

func TestSanitizeToolCallArguments_EmptyMapNoop(t *testing.T) {
	if sanitizeToolCallArguments("search", map[string]any{}) {
		t.Errorf("empty map reported as changed")
	}
	if sanitizeToolCallArguments("search", nil) {
		t.Errorf("nil map reported as changed")
	}
}

func TestSanitizeToolCallArguments_GemmaEndToEnd(t *testing.T) {
	// Exactly the payload shape we saw in /tmp/ux-probe-gemma-post
	// ctx-low__stream-true.json for the fetch tool.
	raw := `{"url":"<|\"|https://local.test/cats/gazette<|\"|>"}`
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("unmarshal captured fetch args: %v", err)
	}
	if !sanitizeToolCallArguments("fetch", args) {
		t.Fatalf("captured gemma args reported as clean")
	}
	got, _ := args["url"].(string)
	if _, err := url.Parse(got); err != nil {
		t.Errorf("repaired URL fails url.Parse: %q err=%v", got, err)
	}
	if got != "https://local.test/cats/gazette" {
		t.Errorf("repaired URL mismatch: %q", got)
	}
}

func TestExtractFirstJSONObject_Clean(t *testing.T) {
	got, ok := extractFirstJSONObject(`{"a":1}`)
	if !ok || got != `{"a":1}` {
		t.Errorf("clean object: got=%q ok=%v", got, ok)
	}
}

func TestExtractFirstJSONObject_TrailingProse(t *testing.T) {
	// The pi-mono #952 / kimi-k2-thinking shape: complete JSON object
	// followed by reasoning/prose that should be discarded.
	got, ok := extractFirstJSONObject(`{"action":"read","path":"/etc/hostname"}Let me read this file...`)
	if !ok || got != `{"action":"read","path":"/etc/hostname"}` {
		t.Errorf("trailing prose: got=%q ok=%v", got, ok)
	}
}

func TestExtractFirstJSONObject_NestedThenProse(t *testing.T) {
	got, ok := extractFirstJSONObject(`{"a":{"b":2}} Then I'll ...`)
	if !ok || got != `{"a":{"b":2}}` {
		t.Errorf("nested+prose: got=%q ok=%v", got, ok)
	}
}

func TestExtractFirstJSONObject_RespectsQuotedBrace(t *testing.T) {
	// A closing brace inside a string literal must not end the object.
	in := `{"cmd":"echo }"}`
	got, ok := extractFirstJSONObject(in)
	if !ok || got != in {
		t.Errorf("quoted brace mishandled: got=%q ok=%v", got, ok)
	}
}

func TestExtractFirstJSONObject_RespectsEscapeInString(t *testing.T) {
	// A backslash-escaped quote must not prematurely end the string.
	in := `{"cmd":"she said \"hi\" then }"}`
	got, ok := extractFirstJSONObject(in)
	if !ok || got != in {
		t.Errorf("escaped quote mishandled: got=%q ok=%v", got, ok)
	}
}

func TestExtractFirstJSONObject_Unterminated(t *testing.T) {
	got, ok := extractFirstJSONObject(`{"cmd":"unterminated`)
	if ok || got != "" {
		t.Errorf("unterminated should fail: got=%q ok=%v", got, ok)
	}
}

func TestExtractFirstJSONObject_DoesNotStartWithBrace(t *testing.T) {
	got, ok := extractFirstJSONObject(`[1,2,3]`)
	if ok || got != "" {
		t.Errorf("array input should fail: got=%q ok=%v", got, ok)
	}
	got, ok = extractFirstJSONObject(``)
	if ok || got != "" {
		t.Errorf("empty input should fail: got=%q ok=%v", got, ok)
	}
}

func TestSanitizeToolCallArgumentsJSON_CleanPassthrough(t *testing.T) {
	in := []byte(`{"a":1,"b":"x"}`)
	out, changed := sanitizeToolCallArgumentsJSON(in)
	if changed {
		t.Errorf("clean input reported as changed")
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("clean input mutated: in=%q out=%q", in, out)
	}
}

func TestSanitizeToolCallArgumentsJSON_RepairsTrailingProse(t *testing.T) {
	in := []byte(`{"query":"cats","max_results":5}Now searching...`)
	out, changed := sanitizeToolCallArgumentsJSON(in)
	if !changed {
		t.Fatalf("expected repair to fire")
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Errorf("repaired output not valid JSON: %v", err)
	}
	if parsed["query"] != "cats" || parsed["max_results"] != float64(5) {
		t.Errorf("repaired args wrong: %v", parsed)
	}
}

func TestSanitizeToolCallArgumentsJSON_UnrepairableLeftAlone(t *testing.T) {
	// Truncated JSON is unrepairable; the function must return the input
	// unchanged rather than inventing content.
	in := []byte(`{"cmd":"unterminated`)
	out, changed := sanitizeToolCallArgumentsJSON(in)
	if changed {
		t.Errorf("unrepairable input claimed as changed")
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("unrepairable input mutated: in=%q out=%q", in, out)
	}
}

func TestSanitizeToolCallArgumentsJSON_EmptyInput(t *testing.T) {
	out, changed := sanitizeToolCallArgumentsJSON(nil)
	if changed || out != nil {
		t.Errorf("nil in: out=%v changed=%v", out, changed)
	}
	out, changed = sanitizeToolCallArgumentsJSON([]byte{})
	if changed || len(out) != 0 {
		t.Errorf("empty in: out=%v changed=%v", out, changed)
	}
}

func TestSanitizeToolCallArgumentsJSON_KimiCapturedShape(t *testing.T) {
	// Synthesized to match the kimi-k2-5 streaming failure we captured
	// earlier this session: "Expecting ',' delimiter: line 1 column 82".
	// The common pattern is a valid JSON object followed by reasoning
	// text that starts with whitespace or prose.
	in := []byte(`{"query":"Neighborhood Cat Gazette breakfast routines","max_results":5}  Now I'll review these results carefully.`)
	out, changed := sanitizeToolCallArgumentsJSON(in)
	if !changed {
		t.Fatalf("expected repair to fire on captured kimi shape")
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Errorf("kimi repair output invalid: %v", err)
	}
	if parsed["query"] != "Neighborhood Cat Gazette breakfast routines" {
		t.Errorf("kimi repair lost query: %v", parsed)
	}
}

func TestSanitizeAssistantToolCallsInMessages_CleanHistoryUnchanged(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "you are helpful"},
		map[string]any{"role": "user", "content": "find cats"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "search",
						"arguments": `{"query":"cats"}`,
					},
				},
			},
		},
	}
	snapshot := deepCopyJSON(t, messages)
	if changed := sanitizeAssistantToolCallsInMessages(messages); changed != 0 {
		t.Errorf("clean history reported %d changes", changed)
	}
	if !reflect.DeepEqual(messages, snapshot) {
		t.Errorf("clean history mutated")
	}
}

func TestSanitizeAssistantToolCallsInMessages_ReplacesMalformedWithEmpty(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "do something"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": `{"cmd":"unterminated`,
					},
				},
			},
		},
	}
	changed := sanitizeAssistantToolCallsInMessages(messages)
	if changed != 1 {
		t.Fatalf("expected 1 change, got %d", changed)
	}
	assistant, _ := messages[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if got := fn["arguments"]; got != "{}" {
		t.Errorf("malformed args not replaced with {}: got=%q", got)
	}
}

func TestSanitizeAssistantToolCallsInMessages_RepairsRecoverableArgs(t *testing.T) {
	// Trailing prose after a valid JSON object: our json_repair can
	// recover it, so the history guard should prefer the repaired
	// object over replacing with {}.
	messages := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "search",
						"arguments": `{"query":"cats","max_results":5} Now searching...`,
					},
				},
			},
		},
	}
	if changed := sanitizeAssistantToolCallsInMessages(messages); changed != 1 {
		t.Fatalf("expected 1 change, got %d", changed)
	}
	assistant, _ := messages[0].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	got, _ := fn["arguments"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Errorf("repaired history arg invalid: got=%q err=%v", got, err)
	}
	if parsed["query"] != "cats" || parsed["max_results"] != float64(5) {
		t.Errorf("repaired history args wrong: %v", parsed)
	}
}

func TestSanitizeAssistantToolCallsInMessages_IgnoresNonAssistantRoles(t *testing.T) {
	// Even a malformed arguments string living in a user or tool
	// message must not be touched; those shapes are not actually a
	// thing we produce, but we do not want to start rewriting foreign
	// content even if a caller hands it to us.
	messages := []any{
		map[string]any{
			"role": "user",
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": `{"cmd":"unterminated`,
					},
				},
			},
		},
	}
	snapshot := deepCopyJSON(t, messages)
	if changed := sanitizeAssistantToolCallsInMessages(messages); changed != 0 {
		t.Errorf("non-assistant role was sanitized: changed=%d", changed)
	}
	if !reflect.DeepEqual(messages, snapshot) {
		t.Errorf("non-assistant role was mutated")
	}
}

func TestSanitizeAssistantToolCallsInMessages_MultipleCallsInSameMessage(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "search",
						"arguments": `{"query":"cats"}`,
					},
				},
				map[string]any{
					"id":   "call_2",
					"type": "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": `{"cmd":"`,
					},
				},
				map[string]any{
					"id":   "call_3",
					"type": "function",
					"function": map[string]any{
						"name":      "read",
						"arguments": `{"path":"/a/b"}`,
					},
				},
			},
		},
	}
	changed := sanitizeAssistantToolCallsInMessages(messages)
	if changed != 1 {
		t.Fatalf("expected 1 change across 3 calls, got %d", changed)
	}
	assistant, _ := messages[0].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if fn, _ := calls[0].(map[string]any)["function"].(map[string]any); fn["arguments"] != `{"query":"cats"}` {
		t.Errorf("first call unexpectedly changed: %v", fn["arguments"])
	}
	if fn, _ := calls[1].(map[string]any)["function"].(map[string]any); fn["arguments"] != "{}" {
		t.Errorf("second (malformed) call not replaced: %v", fn["arguments"])
	}
	if fn, _ := calls[2].(map[string]any)["function"].(map[string]any); fn["arguments"] != `{"path":"/a/b"}` {
		t.Errorf("third call unexpectedly changed: %v", fn["arguments"])
	}
}

func TestSanitizeAssistantToolCallsInMessages_Idempotent(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": `{"cmd":"`,
					},
				},
			},
		},
	}
	firstPass := sanitizeAssistantToolCallsInMessages(messages)
	snapshot := deepCopyJSON(t, messages)
	secondPass := sanitizeAssistantToolCallsInMessages(messages)
	if secondPass != 0 {
		t.Errorf("second pass reported changes: %d", secondPass)
	}
	if !reflect.DeepEqual(messages, snapshot) {
		t.Errorf("second pass mutated messages")
	}
	if firstPass != 1 {
		t.Errorf("first pass count wrong: %d", firstPass)
	}
}

// deepCopyJSON round-trips a JSON value through Marshal/Unmarshal so a
// test can snapshot the starting state and then reliably detect whether a
// helper mutated the original map in place.
func deepCopyJSON(t *testing.T, v any) any {
	t.Helper()
	bs, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("deepCopyJSON marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(bs, &out); err != nil {
		t.Fatalf("deepCopyJSON unmarshal: %v", err)
	}
	return out
}
