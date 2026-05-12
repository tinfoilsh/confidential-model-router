package toolruntime

import (
	"reflect"
	"sort"
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

func setKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
