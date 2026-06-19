package toolruntime

import (
	"reflect"
	"testing"
)

// TestStripClientSyntheticResponseItems pins that router-synthesized
// output-item types are dropped from a client-echoed input array while
// real conversation items survive. Covers both synthesized families
// (web_search_call, code_interpreter_call) and the pass-through cases.
func TestStripClientSyntheticResponseItems(t *testing.T) {
	t.Parallel()

	userMsg := map[string]any{"type": "message", "role": "user"}
	assistantMsg := map[string]any{"type": "message", "role": "assistant"}
	fnOutput := map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "{}"}

	// web_search_call without an id (what Codex echoes back) and with one.
	wsNoID := map[string]any{"type": "web_search_call", "status": "completed"}
	wsWithID := map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed"}
	ciCall := map[string]any{"type": "code_interpreter_call", "id": "ci_1", "status": "completed"}

	got := stripClientSyntheticResponseItems([]any{userMsg, wsNoID, assistantMsg, wsWithID, ciCall, fnOutput})
	want := []any{userMsg, assistantMsg, fnOutput}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stripped = %v, want %v", got, want)
	}

	// Non-slice input passes through unchanged.
	if got := stripClientSyntheticResponseItems("not a slice"); got != "not a slice" {
		t.Fatalf("expected non-slice passthrough, got %v", got)
	}
	if got := stripClientSyntheticResponseItems(nil); got != nil {
		t.Fatalf("expected nil passthrough, got %v", got)
	}
	if got := stripClientSyntheticResponseItems([]any{}); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("expected empty slice, got %v", got)
	}
}
