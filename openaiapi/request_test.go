package openaiapi

import (
	"encoding/json"
	"testing"
)

func TestParseRequestPreservesStreamingUsageIntent(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"stream": true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	req, handled, err := ParseRequest("/v1/chat/completions", nil, raw)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if !handled {
		t.Fatal("expected typed request handling")
	}
	if err := req.EnableContinuousUsageStats(); err != nil {
		t.Fatalf("EnableContinuousUsageStats: %v", err)
	}
	if !req.ClientRequestedStreamingUsage {
		t.Fatal("expected client requested usage flag to remain true")
	}

	bodyBytes, err := req.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	streamOptions, ok := body["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("expected stream_options object, got %#v", body["stream_options"])
	}
	value, ok := streamOptions["continuous_usage_stats"].(bool)
	if !ok || !value {
		t.Fatalf("expected continuous_usage_stats to be enabled, got %#v", streamOptions["continuous_usage_stats"])
	}
}
