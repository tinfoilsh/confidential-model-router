package citations

import (
	"testing"
)

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
