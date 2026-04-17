package toolruntime

import (
	"reflect"
	"testing"
)

func TestParseChatWebSearchOptions_AllFields(t *testing.T) {
	body := map[string]any{
		"web_search_options": map[string]any{
			"search_context_size": "high",
			"user_location": map[string]any{
				"type": "approximate",
				"approximate": map[string]any{
					"country": "gb",
					"city":    "London",
				},
			},
			"filters": map[string]any{
				"allowed_domains": []any{"python.org", "BBC.co.uk", "python.org"},
			},
		},
	}
	got := parseChatWebSearchOptions(body)
	if got.searchContextSize != "high" {
		t.Errorf("search_context_size = %q, want high", got.searchContextSize)
	}
	if got.userLocationCountry != "GB" {
		t.Errorf("user_location_country = %q, want GB", got.userLocationCountry)
	}
	want := []string{"python.org", "bbc.co.uk"}
	if !reflect.DeepEqual(got.allowedDomains, want) {
		t.Errorf("allowed_domains = %v, want %v", got.allowedDomains, want)
	}
}

func TestParseChatWebSearchOptions_TopLevelFiltersFallback(t *testing.T) {
	body := map[string]any{
		"filters": map[string]any{
			"allowed_domains": []any{"python.org"},
		},
	}
	got := parseChatWebSearchOptions(body)
	want := []string{"python.org"}
	if !reflect.DeepEqual(got.allowedDomains, want) {
		t.Errorf("allowed_domains = %v, want %v", got.allowedDomains, want)
	}
}

func TestParseResponsesWebSearchOptions_FromToolEntry(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "other"},
			map[string]any{
				"type":                "web_search",
				"search_context_size": "low",
				"user_location": map[string]any{
					"approximate": map[string]any{"country": "de"},
				},
				"filters": map[string]any{
					"allowed_domains": []any{"wikipedia.org"},
				},
			},
		},
	}
	got := parseResponsesWebSearchOptions(body)
	if got.searchContextSize != "low" {
		t.Errorf("search_context_size = %q, want low", got.searchContextSize)
	}
	if got.userLocationCountry != "DE" {
		t.Errorf("user_location_country = %q, want DE", got.userLocationCountry)
	}
	want := []string{"wikipedia.org"}
	if !reflect.DeepEqual(got.allowedDomains, want) {
		t.Errorf("allowed_domains = %v, want %v", got.allowedDomains, want)
	}
}

func TestWebSearchOptions_MaxResults(t *testing.T) {
	cases := []struct {
		size string
		want int
	}{
		{"low", searchContextResultsLow},
		{"LOW", searchContextResultsLow},
		{"medium", searchContextResultsMedium},
		{"high", searchContextResultsHigh},
		{"", 0},
		{"unknown", 0},
	}
	for _, tc := range cases {
		opts := webSearchOptions{searchContextSize: tc.size}
		if got := opts.maxResults(); got != tc.want {
			t.Errorf("maxResults(%q) = %d, want %d", tc.size, got, tc.want)
		}
	}
}

func TestApplyToSearchArgs_FillsUnsetFieldsOnly(t *testing.T) {
	opts := webSearchOptions{
		userLocationCountry: "GB",
		allowedDomains:      []string{"python.org"},
		searchContextSize:   "high",
	}

	args := map[string]any{"query": "python tutorial"}
	opts.applyToSearchArgs(args)

	if args["query"] != "python tutorial" {
		t.Errorf("query mutated unexpectedly: %v", args["query"])
	}
	if args["max_results"] != searchContextResultsHigh {
		t.Errorf("max_results = %v, want %d", args["max_results"], searchContextResultsHigh)
	}
	if args["user_location_country"] != "GB" {
		t.Errorf("user_location_country = %v", args["user_location_country"])
	}
	if !reflect.DeepEqual(args["allowed_domains"], []any{"python.org"}) {
		t.Errorf("allowed_domains = %v", args["allowed_domains"])
	}
}

func TestApplyToSearchArgs_RespectsModelOverrides(t *testing.T) {
	opts := webSearchOptions{
		userLocationCountry: "US",
		allowedDomains:      []string{"example.com"},
		searchContextSize:   "low",
	}

	args := map[string]any{
		"query":                 "weather",
		"max_results":           42,
		"user_location_country": "FR",
		"allowed_domains":       []any{"wikipedia.org"},
	}
	opts.applyToSearchArgs(args)

	if args["max_results"] != 42 {
		t.Errorf("max_results overridden, want 42 got %v", args["max_results"])
	}
	if args["user_location_country"] != "FR" {
		t.Errorf("user_location_country overridden, want FR got %v", args["user_location_country"])
	}
	if !reflect.DeepEqual(args["allowed_domains"], []any{"wikipedia.org"}) {
		t.Errorf("allowed_domains overridden: got %v", args["allowed_domains"])
	}
}

func TestApplyToFetchArgs_OnlyForwardsDomain(t *testing.T) {
	opts := webSearchOptions{
		userLocationCountry: "GB",
		allowedDomains:      []string{"python.org"},
		searchContextSize:   "high",
	}
	args := map[string]any{"urls": []any{"https://docs.python.org/3/"}}
	opts.applyToFetchArgs(args)

	if _, set := args["max_results"]; set {
		t.Errorf("fetch args should not receive max_results")
	}
	if _, set := args["user_location_country"]; set {
		t.Errorf("fetch args should not receive user_location_country")
	}
	if !reflect.DeepEqual(args["allowed_domains"], []any{"python.org"}) {
		t.Errorf("allowed_domains = %v", args["allowed_domains"])
	}
}

func TestApplyWebSearchOptionsToToolCall_DispatchesPerTool(t *testing.T) {
	opts := webSearchOptions{
		userLocationCountry: "US",
		allowedDomains:      []string{"example.com"},
		searchContextSize:   "medium",
	}
	searchArgs := map[string]any{"query": "q"}
	applyWebSearchOptionsToToolCall("search", searchArgs, opts)
	if searchArgs["max_results"] != searchContextResultsMedium {
		t.Errorf("search max_results = %v", searchArgs["max_results"])
	}

	fetchArgs := map[string]any{"urls": []any{"https://example.com/"}}
	applyWebSearchOptionsToToolCall("fetch", fetchArgs, opts)
	if _, set := fetchArgs["max_results"]; set {
		t.Errorf("fetch should not get max_results")
	}
	if !reflect.DeepEqual(fetchArgs["allowed_domains"], []any{"example.com"}) {
		t.Errorf("fetch allowed_domains = %v", fetchArgs["allowed_domains"])
	}

	otherArgs := map[string]any{"foo": "bar"}
	applyWebSearchOptionsToToolCall("unknown", otherArgs, opts)
	if len(otherArgs) != 1 {
		t.Errorf("unknown tool args should not be mutated, got %v", otherArgs)
	}
}

func TestParseSafetyOptIns_Presence(t *testing.T) {
	body := map[string]any{
		"pii_check_options":              map[string]any{},
		"prompt_injection_check_options": map[string]any{},
	}
	got := parseSafetyOptIns(body)
	if got.pii == nil || !*got.pii {
		t.Errorf("pii opt-in = %v, want pointer to true", got.pii)
	}
	if got.injection == nil || !*got.injection {
		t.Errorf("injection opt-in = %v, want pointer to true", got.injection)
	}
}

func TestParseSafetyOptIns_Absence(t *testing.T) {
	got := parseSafetyOptIns(map[string]any{})
	if got.pii != nil {
		t.Errorf("pii opt-in = %v, want nil", got.pii)
	}
	if got.injection != nil {
		t.Errorf("injection opt-in = %v, want nil", got.injection)
	}
}

func TestParseChatWebSearchOptions_SafetyOptIns(t *testing.T) {
	body := map[string]any{
		"web_search_options":             map[string]any{},
		"pii_check_options":              map[string]any{},
		"prompt_injection_check_options": map[string]any{},
	}
	got := parseChatWebSearchOptions(body)
	if got.piiCheck == nil || !*got.piiCheck {
		t.Errorf("piiCheck = %v, want pointer to true", got.piiCheck)
	}
	if got.injectionCheck == nil || !*got.injectionCheck {
		t.Errorf("injectionCheck = %v, want pointer to true", got.injectionCheck)
	}
}

func TestParseResponsesWebSearchOptions_SafetyOptIns(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "web_search"},
		},
		"pii_check_options":              map[string]any{},
		"prompt_injection_check_options": map[string]any{},
	}
	got := parseResponsesWebSearchOptions(body)
	if got.piiCheck == nil || !*got.piiCheck {
		t.Errorf("piiCheck = %v, want pointer to true", got.piiCheck)
	}
	if got.injectionCheck == nil || !*got.injectionCheck {
		t.Errorf("injectionCheck = %v, want pointer to true", got.injectionCheck)
	}
}
