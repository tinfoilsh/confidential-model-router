package main

import (
	"strings"
	"testing"
)

func TestEstimatePromptTokens_TextOnly(t *testing.T) {
	body := map[string]any{
		"model": "glm-5-2",
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("hello world ", 100)}, // 1200 chars
		},
	}

	got := estimatePromptTokens(body)
	if got < 300 || got > 320 {
		t.Fatalf("expected ~300 tokens for 1200 chars of text, got %d", got)
	}
}

func TestEstimatePromptTokens_DataURLCountsFlat(t *testing.T) {
	// A 1MB data-URL image must count as mediaPartTokens, not bytes/4.
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "describe this image"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + strings.Repeat("iVBORw0KGgo=", 100000),
				}},
			}},
		},
	}

	got := estimatePromptTokens(body)
	if got > mediaPartTokens+100 {
		t.Fatalf("1MB image should estimate near %d flat, got %d", mediaPartTokens, got)
	}
}

func TestEstimatePromptTokens_RawBase64CountsFlat(t *testing.T) {
	// Audio parts carry raw base64 without a data: prefix.
	blob := strings.Repeat("UklGRiQAAABXQVZF", 10000) // 160k chars, base64 alphabet
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_audio", "input_audio": map[string]any{
					"data": blob, "format": "wav",
				}},
			}},
		},
	}

	got := estimatePromptTokens(body)
	if got > mediaPartTokens+100 {
		t.Fatalf("raw base64 audio should estimate near %d flat, got %d", mediaPartTokens, got)
	}
}

func TestEstimatePromptTokens_LongProseIsNotMistakenForBase64(t *testing.T) {
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200) // 9000 chars
	got := estimatePromptTokens(map[string]any{"content": prose})
	want := int64(len(prose)) / estBytesPerToken
	if got != want {
		t.Fatalf("long prose should count as text (%d), got %d", want, got)
	}
}
