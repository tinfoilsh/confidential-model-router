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

func TestEstimatePromptTokens_FragmentedTextStillCharged(t *testing.T) {
	// 400 three-char parts must not each truncate to zero.
	parts := make([]any, 400)
	for i := range parts {
		parts[i] = map[string]any{"type": "text", "text": "abc"}
	}
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": parts}},
	}

	if got := estimatePromptTokens(body); got < 400 {
		t.Fatalf("fragmented content should charge at least 1 token per part, got %d", got)
	}
}

func TestEstimatePromptTokens_AlphanumericHeadedProseIsNotBase64(t *testing.T) {
	// Prose whose first 128+ chars are base64-alphabet must still count as
	// text once a later character breaks the pattern.
	s := strings.Repeat("a", 200) + " " + strings.Repeat("normal prose follows here. ", 200)
	got := estimatePromptTokens(map[string]any{"content": s})
	if got <= mediaPartTokens {
		t.Fatalf("prose with an alphanumeric head should count as text (~%d), got %d", len(s)/estBytesPerToken, got)
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
