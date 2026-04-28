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

func TestResolveHarmonyCitations(t *testing.T) {
	state := &State{NextIndex: 1, Harmony: true}
	state.Record("https://example.com/a", "Source A")
	state.Record("https://example.com/b", "Source B")

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "full harmony with line range",
			input: "claim【1†L1-L3】.",
			want:  "claim[Source A](https://example.com/a).",
		},
		{
			name:  "full harmony single line",
			input: "claim【2†L4】.",
			want:  "claim[Source B](https://example.com/b).",
		},
		{
			name:  "bare cursor",
			input: "claim【1】.",
			want:  "claim[Source A](https://example.com/a).",
		},
		{
			name:  "bare cursor second source",
			input: "claim【2】.",
			want:  "claim[Source B](https://example.com/b).",
		},
		{
			name:  "mixed bare and full",
			input: "first【1】 second【2†L1-L2】.",
			want:  "first[Source A](https://example.com/a) second[Source B](https://example.com/b).",
		},
		{
			name:  "unknown cursor unchanged",
			input: "claim【99】.",
			want:  "claim【99】.",
		},
		{
			name:  "not harmony when disabled",
			input: "claim【1†L1】.",
			want:  "claim【1†L1】.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := state
			if tt.name == "not harmony when disabled" {
				s = &State{NextIndex: 1, Harmony: false}
				s.Record("https://example.com/a", "Source A")
			}
			if got := s.ResolveHarmonyCitations(tt.input); got != tt.want {
				t.Errorf("ResolveHarmonyCitations(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
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

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name     string
		inputs   []string
		expected string
	}{
		{
			name: "trailing slash differences collapse",
			inputs: []string{
				"https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402",
				"https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402/",
			},
			expected: "https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402",
		},
		{
			name: "www prefix collapses",
			inputs: []string{
				"https://theguardian.com/environment/2026/apr/17/article",
				"https://www.theguardian.com/environment/2026/apr/17/article",
			},
			expected: "https://theguardian.com/environment/2026/apr/17/article",
		},
		{
			name: "scheme and host casing collapse",
			inputs: []string{
				"https://apnews.com/article/x",
				"HTTPS://APNEWS.COM/article/x",
			},
			expected: "https://apnews.com/article/x",
		},
		{
			name: "utm tracking params are dropped",
			inputs: []string{
				"https://example.com/article",
				"https://example.com/article?utm_source=exa&utm_medium=web",
				"https://example.com/article?utm_campaign=launch",
			},
			expected: "https://example.com/article",
		},
		{
			name: "fragment is dropped",
			inputs: []string{
				"https://example.com/article",
				"https://example.com/article#section-2",
			},
			expected: "https://example.com/article",
		},
		{
			name: "invisible trailing runes are stripped",
			inputs: []string{
				"https://example.com/article",
				"https://example.com/article\u200B",
				"https://example.com/article\uFEFF",
				"https://example.com/article\u200E",
			},
			expected: "https://example.com/article",
		},
		{
			name: "non-tracking query params are preserved and sorted",
			inputs: []string{
				"https://example.com/search?q=apple&lang=en",
				"https://example.com/search?lang=en&q=apple",
			},
			expected: "https://example.com/search?lang=en&q=apple",
		},
		{
			name: "mixed real-world differences collapse",
			inputs: []string{
				"https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402",
				"HTTPS://www.Apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402/?utm_source=exa#top\u200B",
			},
			expected: "https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402",
		},
		{
			name: "root path slash is preserved",
			inputs: []string{
				"https://example.com/",
			},
			expected: "https://example.com/",
		},
		{
			name: "empty input stays empty",
			inputs: []string{
				"",
				"   ",
			},
			expected: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, input := range tc.inputs {
				got := NormalizeURL(input)
				if got != tc.expected {
					t.Errorf("NormalizeURL(%q) = %q, want %q", input, got, tc.expected)
				}
			}
		})
	}
}

func TestMatchesForToleratesURLDifferences(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402", "AP News")

	text := "Pipeline repairs are complete [Source: AP News](https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402/)."
	matches := state.MatchesFor(text)

	if len(matches) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(matches))
	}
	if matches[0].Source.URL != "https://apnews.com/article/ukraine-druzhba-pipeline-7dfc9574bf95a69eda13b1440171e402/" {
		t.Errorf("annotation carries url %q instead of the model's url", matches[0].Source.URL)
	}
	if matches[0].Source.Title != "AP News" {
		t.Errorf("annotation title = %q, want %q", matches[0].Source.Title, "AP News")
	}
}

func TestResolveSourceToleratesURLDifferences(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://example.com/article", "Example")

	got, ok := state.ResolveSource("https://www.example.com/article/?utm_source=test")
	if !ok {
		t.Fatal("expected ResolveSource to match via normalization")
	}
	if got.URL != "https://example.com/article" {
		t.Errorf("ResolveSource returned url %q, want the recorded url", got.URL)
	}
}
