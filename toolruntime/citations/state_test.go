package citations

import (
	"strings"
	"testing"
)

// TestMarkdownCitationsEmitAnnotations verifies that inline markdown links
// whose URLs were recorded during tool execution turn into url_citation
// annotations spanning the visible label.
func TestMarkdownCitationsEmitAnnotations(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://example.com/a", "Source A")
	state.Record("https://example.com/b", "Source B")

	content := "First claim [Source A](https://example.com/a). Later claim [Source B](https://example.com/b)."
	nested := state.NestedAnnotationsFor(content)

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

func TestMarkdownCitationsIgnoreUnknownURLs(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://known.example/a", "Known Source")

	content := "Real cite [Known](https://known.example/a) and fake cite [Fake](https://fabricated.example/x)."
	nested := state.NestedAnnotationsFor(content)

	if len(nested) != 1 {
		t.Fatalf("expected exactly 1 annotation (unknown URL dropped), got %d", len(nested))
	}
	ann, _ := nested[0].(map[string]any)
	inner, _ := ann["url_citation"].(map[string]any)
	if inner["url"] != "https://known.example/a" {
		t.Errorf("wrong annotation url=%v", inner["url"])
	}
}

func TestMarkdownCitationsRepeatedOccurrences(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://example.com/same", "Same Source")

	content := "First [Same Source](https://example.com/same) and again [Same Source](https://example.com/same)."
	nested := state.NestedAnnotationsFor(content)

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

func TestMarkdownCitationsFlatShape(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://example.com/a", "Source A")
	state.Record("https://example.com/b", "Source B")

	content := "Claim [A](https://example.com/a). Another [B](https://example.com/b)."
	flat := state.FlatAnnotationsFor(content)

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

func TestMarkdownCitationsNoLinks(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://example.com/a", "Source A")

	if got := state.NestedAnnotationsFor("plain text without citations"); len(got) != 0 {
		t.Errorf("expected no annotations, got %d", len(got))
	}
}

func TestNormalizeLinksRewritesFullwidthBrackets(t *testing.T) {
	input := "A claim 【Example page】(https://example.com/page) stands."
	want := "A claim [Example page](https://example.com/page) stands."
	if got := NormalizeLinks(input); got != want {
		t.Errorf("NormalizeLinks(%q) = %q, want %q", input, got, want)
	}

	if unchanged := "no brackets here"; NormalizeLinks(unchanged) != unchanged {
		t.Errorf("plain text should not be modified")
	}
}

func TestNormalizeLinksRewritesMixedBrackets(t *testing.T) {
	input := "A claim 【Example page](https://example.com/page) stands."
	want := "A claim [Example page](https://example.com/page) stands."
	if got := NormalizeLinks(input); got != want {
		t.Errorf("NormalizeLinks(%q) = %q, want %q", input, got, want)
	}
}

func TestMarkdownCitationsUseCodePointOffsets(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://example.com/page", "Page")

	content := "Thin\u202fspace\u202fthen [Page](https://example.com/page)."
	nested := state.NestedAnnotationsFor(content)
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
