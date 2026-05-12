package toolruntime

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestExtractAndStripAutoContinueChatTools verifies the helper records
// flagged tools and removes the router-internal flag from the upstream
// tools array.
func TestExtractAndStripAutoContinueChatTools(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":                         "render_stat_cards",
				"description":                  "stat cards",
				"x-tinfoil-tool-auto-continue": true,
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "weather",
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":                         "render_chart",
				"x-tinfoil-tool-auto-continue": true,
			},
		},
	}

	got := extractAndStripAutoContinueChatTools(tools)
	wantNames := []string{"render_chart", "render_stat_cards"}
	gotNames := setKeys(got)
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("extracted names mismatch: got %v want %v", gotNames, wantNames)
	}

	for _, raw := range tools {
		tool := raw.(map[string]any)
		fn := tool["function"].(map[string]any)
		if _, present := fn[autoContinueToolFlag]; present {
			t.Errorf("flag not stripped from %v", fn["name"])
		}
	}
}

func TestHasAutoContinueTools(t *testing.T) {
	if !HasAutoContinueTools("/v1/chat/completions", map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":                         "render_stat_cards",
					"x-tinfoil-tool-auto-continue": true,
				},
			},
		},
	}) {
		t.Fatal("expected flagged chat tool to activate auto-continue runtime")
	}
	if !HasAutoContinueTools("/v1/responses", map[string]any{
		"tools": []any{
			map[string]any{
				"type":                         "function",
				"name":                         "render_stat_cards",
				"x-tinfoil-tool-auto-continue": true,
			},
		},
	}) {
		t.Fatal("expected flagged responses tool to activate auto-continue runtime")
	}
	if HasAutoContinueTools("/v1/chat/completions", map[string]any{
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_order"}}},
	}) {
		t.Fatal("unflagged tool should not activate auto-continue runtime")
	}
}

// TestExtractAndStripAutoContinueResponsesTools mirrors the chat-test for
// the flat /v1/responses tool shape.
func TestExtractAndStripAutoContinueResponsesTools(t *testing.T) {
	tools := []any{
		map[string]any{
			"type":                         "function",
			"name":                         "render_artifact_preview",
			"x-tinfoil-tool-auto-continue": true,
		},
		map[string]any{
			"type": "function",
			"name": "lookup_order",
		},
	}

	got := extractAndStripAutoContinueResponsesTools(tools)
	if _, ok := got["render_artifact_preview"]; !ok {
		t.Fatalf("expected render_artifact_preview to be marked auto-continue, got %v", got)
	}
	if _, ok := got["lookup_order"]; ok {
		t.Fatalf("unflagged tool should not be auto-continue, got %v", got)
	}

	for _, raw := range tools {
		tool := raw.(map[string]any)
		if _, present := tool[autoContinueToolFlag]; present {
			t.Errorf("flag not stripped from %v", tool["name"])
		}
	}
}

// TestExtractAutoContinueEmptyInputs covers degenerate shapes the helper
// must tolerate without panicking.
func TestExtractAutoContinueEmptyInputs(t *testing.T) {
	if got := extractAndStripAutoContinueChatTools(nil); got != nil {
		t.Errorf("nil tools should yield nil set, got %v", got)
	}
	if got := extractAndStripAutoContinueChatTools([]any{}); got != nil {
		t.Errorf("empty tools should yield nil set, got %v", got)
	}
	if got := extractAndStripAutoContinueChatTools([]any{"not a map"}); got != nil {
		t.Errorf("non-map entries should be skipped, got %v", got)
	}
	if got := extractAndStripAutoContinueResponsesTools(nil); got != nil {
		t.Errorf("nil responses tools should yield nil set, got %v", got)
	}
}

// TestSplitClientToolCalls verifies the partition of client-owned tool
// calls into auto-continue and external-host buckets.
func TestSplitClientToolCalls(t *testing.T) {
	autoContinue := map[string]struct{}{
		"render_stat_cards": {},
	}
	calls := []toolCall{
		{id: "1", name: "render_stat_cards"},
		{id: "2", name: "get_order"},
		{id: "3", name: "render_stat_cards"},
	}
	got, external := splitClientToolCalls(autoContinue, calls)
	if len(got) != 2 || got[0].id != "1" || got[1].id != "3" {
		t.Errorf("auto-continue partition wrong: %+v", got)
	}
	if len(external) != 1 || external[0].id != "2" {
		t.Errorf("external partition wrong: %+v", external)
	}
}

// TestSplitClientToolCallsEmpty covers the empty-input fast path.
func TestSplitClientToolCallsEmpty(t *testing.T) {
	if d, e := splitClientToolCalls(nil, nil); d != nil || e != nil {
		t.Fatalf("expected nil, nil for empty inputs; got %v, %v", d, e)
	}
}

// TestCanonicalizeAutoContinueArgumentsUnwrapsDeepSeekShape verifies the
// concrete DeepSeek-V4-Pro non-streaming quirk captured during validation:
// nested arrays/objects emitted as JSON-encoded strings. Round-tripping
// through the canonicaliser must yield native arrays/objects so client
// widget schemas accept them.
func TestCanonicalizeAutoContinueArgumentsUnwrapsDeepSeekShape(t *testing.T) {
	raw := `{"stats":"[{\"label\":\"Human trials\",\"value\":\"20+\"}]"}`
	got := canonicalizeAutoContinueArguments(raw)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("canonicalised arguments not valid JSON: %v (raw=%s)", err, got)
	}
	stats, ok := parsed["stats"].([]any)
	if !ok {
		t.Fatalf("expected stats to be a native array, got %T (%v)", parsed["stats"], parsed["stats"])
	}
	if len(stats) != 1 {
		t.Fatalf("expected one stat row, got %d (%v)", len(stats), stats)
	}
	row, _ := stats[0].(map[string]any)
	if stringValue(row["label"]) != "Human trials" || stringValue(row["value"]) != "20+" {
		t.Fatalf("row contents mangled: %v", row)
	}
}

// TestCanonicalizeAutoContinueArgumentsIdempotent guarantees clean inputs
// pass through unchanged so the unwrap can run on every auto-continue
// response without risking silent shape changes for compliant providers.
func TestCanonicalizeAutoContinueArgumentsIdempotent(t *testing.T) {
	clean := `{"series":[{"label":"Q1","value":10},{"label":"Q2","value":15}]}`
	once := canonicalizeAutoContinueArguments(clean)
	twice := canonicalizeAutoContinueArguments(once)

	var a, b any
	if err := json.Unmarshal([]byte(once), &a); err != nil {
		t.Fatalf("first pass not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(twice), &b); err != nil {
		t.Fatalf("second pass not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("not idempotent: once=%s twice=%s", once, twice)
	}
}

// TestCanonicalizeAutoContinueArgumentsLeavesNonJSONAlone covers the
// invariant that anything outside the JSON-object/array prefix is returned
// verbatim. Avoids surprising downstream consumers when a tool emits a
// malformed payload the router cannot reason about.
func TestCanonicalizeAutoContinueArgumentsLeavesNonJSONAlone(t *testing.T) {
	cases := []string{
		"",
		"plain text",
		"{ unclosed",
		`"already a string"`,
	}
	for _, in := range cases {
		if got := canonicalizeAutoContinueArguments(in); got != in {
			t.Errorf("canonicalise(%q) = %q; want unchanged", in, got)
		}
	}
}

// TestCanonicalizeAutoContinueArgumentsRecursive checks the helper unwraps
// JSON-encoded strings nested several levels deep, which can happen when a
// stringified array contains stringified objects.
func TestCanonicalizeAutoContinueArgumentsRecursive(t *testing.T) {
	raw := `{"outer":"{\"inner\":\"[1,2,3]\"}"}`
	got := canonicalizeAutoContinueArguments(raw)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v (raw=%s)", err, got)
	}
	outer, _ := parsed["outer"].(map[string]any)
	inner, _ := outer["inner"].([]any)
	if len(inner) != 3 {
		t.Fatalf("expected inner array of length 3, got %v", outer)
	}
}

func TestCanonicalizeAutoContinueArgumentsPreservesLargeNumbers(t *testing.T) {
	const large = "9007199254740993"
	raw := `{"id":` + large + `,"nested":"{\"id\":` + large + `}"}`
	got := canonicalizeAutoContinueArguments(raw)
	if !strings.Contains(got, `"id":`+large) {
		t.Fatalf("top-level number was not preserved: %s", got)
	}
	if !strings.Contains(got, `"nested":{"id":`+large+`}`) {
		t.Fatalf("nested number was not preserved: %s", got)
	}
}

// TestFilterChatRawToolCallsCanonicalisesArguments confirms the unwrap
// runs as the auto-continue payload is shaped for the final response.
func TestFilterChatRawToolCallsCanonicalisesArguments(t *testing.T) {
	rawCalls := []any{
		map[string]any{
			"id":   "c1",
			"type": "function",
			"function": map[string]any{
				"name":      "render_stat_cards",
				"arguments": `{"stats":"[{\"label\":\"a\",\"value\":1}]"}`,
			},
		},
	}
	got := filterChatRawToolCalls(rawCalls, []toolCall{{id: "c1", name: "render_stat_cards"}})
	if len(got) != 1 {
		t.Fatalf("expected one filtered call, got %d", len(got))
	}
	call := got[0].(map[string]any)
	fn := call["function"].(map[string]any)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stringValue(fn["arguments"])), &parsed); err != nil {
		t.Fatalf("filtered arguments not valid JSON: %v", err)
	}
	if _, ok := parsed["stats"].([]any); !ok {
		t.Fatalf("stats should be a native array, got %T", parsed["stats"])
	}
}

// TestFilterResponsesOutputItemsCanonicalisesArguments mirrors the chat
// test for the /v1/responses output-items shape.
func TestFilterResponsesOutputItemsCanonicalisesArguments(t *testing.T) {
	items := []any{
		map[string]any{
			"id":        "fc1",
			"type":      "function_call",
			"name":      "render_chart",
			"call_id":   "c1",
			"arguments": `{"series":"[{\"label\":\"Q1\",\"value\":10}]"}`,
		},
	}
	got := filterResponsesOutputItemsForToolCalls(items, []toolCall{{id: "c1", name: "render_chart"}})
	if len(got) != 1 {
		t.Fatalf("expected one filtered item, got %d", len(got))
	}
	item := got[0].(map[string]any)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stringValue(item["arguments"])), &parsed); err != nil {
		t.Fatalf("filtered arguments not valid JSON: %v", err)
	}
	if _, ok := parsed["series"].([]any); !ok {
		t.Fatalf("series should be a native array, got %T", parsed["series"])
	}
}

func setKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
