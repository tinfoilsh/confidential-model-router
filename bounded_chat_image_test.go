package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func boundedImageDataURL(t *testing.T, media string) string {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var err error
	switch media {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	default:
		t.Fatalf("unsupported fixture format %q", media)
	}
	if err != nil {
		t.Fatal(err)
	}
	return "data:image/" + media + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestBoundedChatImageShape(t *testing.T) {
	pngURL := boundedImageDataURL(t, "png")
	jpegURL := boundedImageDataURL(t, "jpeg")
	for _, tc := range []struct {
		name, role string
		parts      []any
		want       bool
	}{
		{"PNG with text", "user", []any{map[string]any{"type": "text", "text": "describe"}, boundedImagePart(pngURL)}, true},
		{"multiple images", "user", []any{boundedImagePart(pngURL), boundedImagePart(jpegURL)}, true},
		{"system image", "system", []any{boundedImagePart(pngURL)}, false},
		{"developer image", "developer", []any{boundedImagePart(pngURL)}, false},
		{"assistant image", "assistant", []any{boundedImagePart(pngURL)}, false},
		{"tool image", "tool", []any{boundedImagePart(pngURL)}, false},
		{"remote image", "user", []any{boundedImagePart("https://example.invalid/image.png")}, false},
		{"bad base64", "user", []any{boundedImagePart(pngURL + "?")}, false},
		{"noncanonical base64", "user", []any{boundedImagePart(pngURL + "\n")}, false},
		{"empty image", "user", []any{boundedImagePart("data:image/png;base64,")}, false},
		{"wrong signature", "user", []any{boundedImagePart("data:image/jpeg;base64," + pngURL[len("data:image/png;base64,"):])}, false},
		{"SVG", "user", []any{boundedImagePart("data:image/svg+xml;base64,PHN2Zz4=")}, false},
		{"unsupported detail", "user", []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": pngURL, "detail": "original"}}}, false},
		{"image extension", "user", []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": pngURL, "extra": true}}}, false},
		{"image URL string shorthand", "user", []any{map[string]any{"type": "image_url", "image_url": pngURL}}, false},
		{"video", "user", []any{map[string]any{"type": "video_url", "video_url": map[string]any{"url": pngURL}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := map[string]any{"role": tc.role, "content": tc.parts}
			if tc.role == "tool" {
				msg["tool_call_id"] = "call_1"
			}
			if got := boundedChatMessage(msg); got != tc.want {
				t.Fatalf("boundedChatMessage() = %t, want %t", got, tc.want)
			}
		})
	}
}

func boundedImagePart(url string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url, "detail": "high"}}
}

func TestBoundedInlineImageValidatesPastSignature(t *testing.T) {
	const prefix = "data:image/png;base64,"
	data := make([]byte, 3073)
	copy(data, "\x89PNG\r\n\x1a\n")
	encoded := base64.StdEncoding.EncodeToString(data)
	paddedChunk := base64.StdEncoding.EncodeToString(data[:3071])
	for _, tc := range []struct {
		name, payload string
		want          bool
	}{
		{"valid multiple chunks", encoded, true},
		{"invalid character after first chunk", encoded[:4096] + "?A==", false},
		{"invalid padding bits after first chunk", encoded[:4096] + "AB==", false},
		{"padding followed by another chunk", paddedChunk + "AAAA", false},
		{"newline after first chunk", encoded[:4096] + "\n" + strings.Repeat("A", 3), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundedInlineImage(map[string]any{"url": prefix + tc.payload}); got != tc.want {
				t.Fatalf("boundedInlineImage() = %t, want %t", got, tc.want)
			}
		})
	}
}
