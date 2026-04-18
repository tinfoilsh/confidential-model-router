package toolruntime

import (
	"strings"
	"testing"
)

// TestMarkdownCitationsEmitAnnotations verifies that inline markdown links
// whose URLs were recorded during tool execution turn into url_citation
// annotations spanning the visible label.
func TestMarkdownCitationsEmitAnnotations(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/a", "Source A")
	state.record("https://example.com/b", "Source B")

	content := "First claim [Source A](https://example.com/a). Later claim [Source B](https://example.com/b)."
	nested := state.nestedAnnotationsFor(content)

	if len(nested) != 2 {
		t.Fatalf("expected 2 annotations, got %d", len(nested))
	}

	expect := []struct {
		label string
		url   string
		title string
	}{
		{"Source A", "https://example.com/a", "Source A"},
		{"Source B", "https://example.com/b", "Source B"},
	}
	for i, want := range expect {
		ann, _ := nested[i].(map[string]any)
		inner, _ := ann["url_citation"].(map[string]any)
		if inner["url"] != want.url {
			t.Errorf("annotations[%d].url = %q, want %q", i, inner["url"], want.url)
		}
		if inner["title"] != want.title {
			t.Errorf("annotations[%d].title = %q, want %q", i, inner["title"], want.title)
		}
		start, _ := inner["start_index"].(int)
		end, _ := inner["end_index"].(int)
		if start < 0 || end <= start {
			t.Errorf("annotations[%d] span invalid: start=%d end=%d", i, start, end)
		}
		if got := content[start:end]; got != want.label {
			t.Errorf("annotations[%d] span text = %q, want label %q", i, got, want.label)
		}
	}
}

// TestMarkdownCitationsIgnoreUnknownURLs drops annotations for links whose URL
// the router never recorded, so the model cannot ship a working citation for a
// hallucinated URL.
func TestMarkdownCitationsIgnoreUnknownURLs(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://known.example/a", "Known Source")

	content := "Real cite [Known](https://known.example/a) and fake cite [Fake](https://fabricated.example/x)."
	nested := state.nestedAnnotationsFor(content)

	if len(nested) != 1 {
		t.Fatalf("expected exactly 1 annotation (unknown URL dropped), got %d", len(nested))
	}
	ann, _ := nested[0].(map[string]any)
	inner, _ := ann["url_citation"].(map[string]any)
	if inner["url"] != "https://known.example/a" {
		t.Errorf("wrong annotation url=%v", inner["url"])
	}
}

// TestMarkdownCitationsRepeatedOccurrences emits one annotation per occurrence
// of the same URL in the text, matching OpenAI's Responses-API behavior of
// reporting every inline citation span.
func TestMarkdownCitationsRepeatedOccurrences(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/same", "Same Source")

	content := "First [Same Source](https://example.com/same) and again [Same Source](https://example.com/same)."
	nested := state.nestedAnnotationsFor(content)

	if len(nested) != 2 {
		t.Fatalf("expected 2 annotations for 2 inline occurrences, got %d", len(nested))
	}
	firstAnn, _ := nested[0].(map[string]any)
	secondAnn, _ := nested[1].(map[string]any)
	firstInner, _ := firstAnn["url_citation"].(map[string]any)
	secondInner, _ := secondAnn["url_citation"].(map[string]any)
	if firstInner["start_index"].(int) >= secondInner["start_index"].(int) {
		t.Errorf("annotations should be in text order; got start=%v, %v",
			firstInner["start_index"], secondInner["start_index"])
	}
}

// TestMarkdownCitationsFlatShape mirrors the Chat-Completions test for the
// Responses-API flat annotation shape so both API surfaces stay in sync.
func TestMarkdownCitationsFlatShape(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/a", "Source A")
	state.record("https://example.com/b", "Source B")

	content := "Claim [A](https://example.com/a). Another [B](https://example.com/b)."
	flat := state.flatAnnotationsFor(content)

	if len(flat) != 2 {
		t.Fatalf("expected 2 flat annotations, got %d", len(flat))
	}
	for _, raw := range flat {
		ann, _ := raw.(map[string]any)
		if ann["type"] != "url_citation" {
			t.Errorf("annotation type = %v, want url_citation", ann["type"])
		}
		if _, ok := ann["url"].(string); !ok {
			t.Errorf("flat annotation missing url: %#v", ann)
		}
		if strings.TrimSpace(ann["title"].(string)) == "" {
			t.Errorf("flat annotation missing title: %#v", ann)
		}
	}
}

// TestMarkdownCitationsNoLinks returns no annotations when the model answered
// without citing anything, even if the router registered sources during tool
// calls.
func TestMarkdownCitationsNoLinks(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/a", "Source A")

	if got := state.nestedAnnotationsFor("plain text without citations"); len(got) != 0 {
		t.Errorf("expected no annotations, got %d", len(got))
	}
}

// TestNormalizeCitationLinksRewritesFullwidthBrackets documents that the
// router rewrites the near-markdown `【label】(url)` shape some models emit
// into canonical ASCII markdown so both the rendered content and the
// annotations line up for downstream consumers.
func TestNormalizeCitationLinksRewritesFullwidthBrackets(t *testing.T) {
	input := "A claim 【Example page】(https://example.com/page) stands."
	want := "A claim [Example page](https://example.com/page) stands."
	if got := normalizeCitationLinks(input); got != want {
		t.Errorf("normalizeCitationLinks(%q) = %q, want %q", input, got, want)
	}

	if unchanged := "no brackets here"; normalizeCitationLinks(unchanged) != unchanged {
		t.Errorf("plain text should not be modified")
	}
}

// TestNormalizeCitationLinksRewritesMixedBrackets covers the gpt-oss shape
// where the opening lenticular fullwidth bracket pairs with an ASCII close
// bracket, for example `【Example page](https://example.com/page)`. Without
// this rewrite the content contains a visible near-link the client cannot
// resolve to an annotation.
func TestNormalizeCitationLinksRewritesMixedBrackets(t *testing.T) {
	input := "A claim 【Example page](https://example.com/page) stands."
	want := "A claim [Example page](https://example.com/page) stands."
	if got := normalizeCitationLinks(input); got != want {
		t.Errorf("normalizeCitationLinks(%q) = %q, want %q", input, got, want)
	}
}

// TestAttachChatCitationsNormalizesContent exercises the end-to-end pathway:
// when attachChatCitations runs, any fullwidth-bracketed links in the
// assistant message content are rewritten into ASCII markdown and surfaced
// as url_citation annotations.
func TestAttachChatCitationsNormalizesContent(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/page", "Example page")

	body := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "A claim 【Example page】(https://example.com/page) stands.",
				},
			},
		},
	}

	attachChatCitations(body, state)

	choice := body["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if !strings.Contains(message["content"].(string), "[Example page](https://example.com/page)") {
		t.Fatalf("content not normalized to ASCII markdown: %v", message["content"])
	}
	annotations, _ := message["annotations"].([]any)
	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(annotations))
	}
	ann, _ := annotations[0].(map[string]any)
	inner, _ := ann["url_citation"].(map[string]any)
	if inner["url"] != "https://example.com/page" {
		t.Errorf("annotation url = %v", inner["url"])
	}
}

// TestMarkdownCitationsUseCodePointOffsets exposes a byte-vs-rune indexing bug:
// when the content contains multi-byte characters (e.g. en spaces, em dashes),
// the annotation start/end indices must point at the correct code point so
// JS/Python clients slicing the content by character get the link label back.
func TestMarkdownCitationsUseCodePointOffsets(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/page", "Page")

	// Pad the text with multi-byte characters before the link so any byte-vs-rune
	// confusion shows up as a shifted span when the test indexes by rune.
	content := "Thin\u202fspace\u202fthen [Page](https://example.com/page)."
	nested := state.nestedAnnotationsFor(content)
	if len(nested) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(nested))
	}
	ann, _ := nested[0].(map[string]any)
	inner, _ := ann["url_citation"].(map[string]any)
	start := inner["start_index"].(int)
	end := inner["end_index"].(int)

	runes := []rune(content)
	if end > len(runes) {
		t.Fatalf("annotation end (%d) beyond rune length (%d)", end, len(runes))
	}
	if got := string(runes[start:end]); got != "Page" {
		t.Errorf("rune-indexed span = %q, want %q", got, "Page")
	}
}
