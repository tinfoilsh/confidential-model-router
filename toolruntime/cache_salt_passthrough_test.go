package toolruntime

import (
	"net/http"
	"testing"
)

// The router injects cache_salt into the request body before the tool loop
// starts, and the loop replays a growing prefix upstream every iteration —
// the strongest case for prefix caching. Every upstream request the loop
// builds must therefore carry the salt so the whole loop stays inside the
// caller's cache namespace. These tests pin that each request builder clones
// the salted body and never strips the field: the initial request, the 2nd+
// continuation, and the streaming variants.

const testSalt = "salt-123"

func TestChatLoopRequestKeepsCacheSalt(t *testing.T) {
	adapter := newChatLoopAdapter(
		map[string]any{"model": "m", "messages": []any{}, "cache_salt": testSalt},
		nil, nil, nil, "m", http.Header{}, nil,
	)
	if got, _ := adapter.buildInitialRequest()["cache_salt"].(string); got != testSalt {
		t.Errorf("chat initial request cache_salt = %q, want %q", got, testSalt)
	}
}

func TestResponsesLoopRequestKeepsCacheSalt(t *testing.T) {
	adapter := newResponsesLoopAdapter(
		map[string]any{"model": "m", "input": []any{}, "cache_salt": testSalt},
		nil, nil, nil, nil,
	)
	if got, _ := adapter.buildInitialRequest()["cache_salt"].(string); got != testSalt {
		t.Errorf("responses initial request cache_salt = %q, want %q", got, testSalt)
	}
}

// The continuation requests carry the maximal prefix (prompt + prior turns +
// tool outputs), so dropping the salt on iteration 2+ is where a regression
// hurts most.

func TestChatLoopContinuationKeepsCacheSalt(t *testing.T) {
	adapter := newChatLoopAdapter(
		map[string]any{"model": "m", "messages": []any{}, "cache_salt": testSalt},
		nil, nil, nil, "m", http.Header{}, nil,
	)
	req := adapter.buildInitialRequest()
	cont := adapter.applyToolOutputs(req, map[string]any{"role": "assistant"}, []toolOutput{{callID: "c1", output: "x"}})
	if got, _ := cont["cache_salt"].(string); got != testSalt {
		t.Errorf("chat continuation cache_salt = %q, want %q", got, testSalt)
	}
}

func TestResponsesLoopContinuationKeepsCacheSalt(t *testing.T) {
	adapter := newResponsesLoopAdapter(
		map[string]any{"model": "m", "input": []any{}, "cache_salt": testSalt},
		nil, nil, nil, nil,
	)
	adapter.buildInitialRequest() // sets a.base, which continuations clone
	cont := adapter.applyToolOutputs(nil, []any{}, []toolOutput{{callID: "c1", output: "x"}})
	if got, _ := cont["cache_salt"].(string); got != testSalt {
		t.Errorf("responses continuation cache_salt = %q, want %q", got, testSalt)
	}
}

func TestChatStreamRequestKeepsCacheSalt(t *testing.T) {
	req, _ := buildChatStreamRequest(
		map[string]any{"model": "m", "messages": []any{}, "cache_salt": testSalt}, nil, nil)
	if got, _ := req["cache_salt"].(string); got != testSalt {
		t.Errorf("chat stream request cache_salt = %q, want %q", got, testSalt)
	}
	if req["stream"] != true {
		t.Error("chat stream request not marked streaming")
	}
}

func TestResponsesStreamRequestKeepsCacheSalt(t *testing.T) {
	req, _ := buildResponsesStreamRequest(
		map[string]any{"model": "m", "input": []any{}, "cache_salt": testSalt}, nil, nil)
	if got, _ := req["cache_salt"].(string); got != testSalt {
		t.Errorf("responses stream request cache_salt = %q, want %q", got, testSalt)
	}
	if req["stream"] != true {
		t.Error("responses stream request not marked streaming")
	}
}
