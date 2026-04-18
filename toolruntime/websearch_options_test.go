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
				"allowed_domains":  []any{"python.org", "BBC.co.uk", "python.org"},
				"excluded_domains": []any{"Example.com", "example.com", "spammer.io"},
			},
			"content_mode":         "text",
			"max_content_chars":    1234.0,
			"category":             "research paper",
			"start_published_date": "2024-01-15",
			"end_published_date":   "2024-06-30T12:00:00Z",
			"max_age_hours":        float64(0),
		},
	}
	got := parseChatWebSearchOptions(body)
	if got.searchContextSize != "high" {
		t.Errorf("search_context_size = %q, want high", got.searchContextSize)
	}
	if got.userLocationCountry != "GB" {
		t.Errorf("user_location_country = %q, want GB", got.userLocationCountry)
	}
	wantAllowed := []string{"python.org", "bbc.co.uk"}
	if !reflect.DeepEqual(got.allowedDomains, wantAllowed) {
		t.Errorf("allowed_domains = %v, want %v", got.allowedDomains, wantAllowed)
	}
	wantExcluded := []string{"example.com", "spammer.io"}
	if !reflect.DeepEqual(got.excludedDomains, wantExcluded) {
		t.Errorf("excluded_domains = %v, want %v", got.excludedDomains, wantExcluded)
	}
	if got.contentMode != "text" {
		t.Errorf("content_mode = %q, want text", got.contentMode)
	}
	if got.maxContentChars != 1234 {
		t.Errorf("max_content_chars = %d, want 1234", got.maxContentChars)
	}
	if got.category != "research paper" {
		t.Errorf("category = %q, want research paper", got.category)
	}
	if got.startPublishedDate != "2024-01-15T00:00:00Z" {
		t.Errorf("start_published_date = %q, want 2024-01-15T00:00:00Z", got.startPublishedDate)
	}
	if got.endPublishedDate != "2024-06-30T12:00:00Z" {
		t.Errorf("end_published_date = %q, want 2024-06-30T12:00:00Z", got.endPublishedDate)
	}
	if got.maxAgeHours == nil || *got.maxAgeHours != 0 {
		t.Errorf("max_age_hours = %v, want pointer to 0", got.maxAgeHours)
	}
}

func TestParseChatWebSearchOptions_TopLevelFiltersFallback(t *testing.T) {
	body := map[string]any{
		"filters": map[string]any{
			"allowed_domains":  []any{"python.org"},
			"excluded_domains": []any{"aggregator.example"},
		},
	}
	got := parseChatWebSearchOptions(body)
	if want := []string{"python.org"}; !reflect.DeepEqual(got.allowedDomains, want) {
		t.Errorf("allowed_domains = %v, want %v", got.allowedDomains, want)
	}
	if want := []string{"aggregator.example"}; !reflect.DeepEqual(got.excludedDomains, want) {
		t.Errorf("excluded_domains = %v, want %v", got.excludedDomains, want)
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
					"allowed_domains":  []any{"wikipedia.org"},
					"excluded_domains": []any{"Spam.io"},
				},
				"content_mode":      "highlights",
				"max_content_chars": float64(900),
				"category":          "news",
				"max_age_hours":     float64(-1),
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
	if want := []string{"wikipedia.org"}; !reflect.DeepEqual(got.allowedDomains, want) {
		t.Errorf("allowed_domains = %v, want %v", got.allowedDomains, want)
	}
	if want := []string{"spam.io"}; !reflect.DeepEqual(got.excludedDomains, want) {
		t.Errorf("excluded_domains = %v, want %v", got.excludedDomains, want)
	}
	if got.contentMode != "highlights" {
		t.Errorf("content_mode = %q, want highlights", got.contentMode)
	}
	if got.maxContentChars != 900 {
		t.Errorf("max_content_chars = %d, want 900", got.maxContentChars)
	}
	if got.category != "news" {
		t.Errorf("category = %q, want news", got.category)
	}
	if got.maxAgeHours == nil || *got.maxAgeHours != -1 {
		t.Errorf("max_age_hours = %v, want pointer to -1", got.maxAgeHours)
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

func TestWebSearchOptions_TierDrivesContentMode(t *testing.T) {
	cases := []struct {
		size         string
		wantMode     string
		wantMaxChars int
	}{
		{"low", contentModeHighlights, searchContextCharsLow},
		{"LOW", contentModeHighlights, searchContextCharsLow},
		{"medium", contentModeHighlights, searchContextCharsMedium},
		{"high", contentModeText, searchContextCharsHigh},
		{"", "", 0},
		{"unknown", "", 0},
	}
	for _, tc := range cases {
		opts := webSearchOptions{searchContextSize: tc.size}
		if got := opts.tierContentMode(); got != tc.wantMode {
			t.Errorf("tierContentMode(%q) = %q, want %q", tc.size, got, tc.wantMode)
		}
		if got := opts.tierMaxContentChars(); got != tc.wantMaxChars {
			t.Errorf("tierMaxContentChars(%q) = %d, want %d", tc.size, got, tc.wantMaxChars)
		}
	}
}

func TestWebSearchOptions_CallerOverrideBeatsTier(t *testing.T) {
	opts := webSearchOptions{
		searchContextSize: "low",
		contentMode:       "text",
		maxContentChars:   4096,
	}
	if got := opts.effectiveContentMode(); got != "text" {
		t.Errorf("effectiveContentMode = %q, want text", got)
	}
	if got := opts.effectiveMaxContentChars(); got != 4096 {
		t.Errorf("effectiveMaxContentChars = %d, want 4096", got)
	}
}

func TestApplyToSearchArgs_ForwardsNewFields(t *testing.T) {
	maxAge := -1
	opts := webSearchOptions{
		searchContextSize:  "high",
		excludedDomains:    []string{"spam.io"},
		category:           "news",
		startPublishedDate: "2024-01-01T00:00:00Z",
		endPublishedDate:   "2024-12-31T00:00:00Z",
		maxAgeHours:        &maxAge,
	}
	args := map[string]any{"query": "ai"}
	opts.applyToSearchArgs(args)

	if args["content_mode"] != contentModeText {
		t.Errorf("content_mode = %v, want %q", args["content_mode"], contentModeText)
	}
	if args["max_content_chars"] != searchContextCharsHigh {
		t.Errorf("max_content_chars = %v, want %d", args["max_content_chars"], searchContextCharsHigh)
	}
	if !reflect.DeepEqual(args["excluded_domains"], []any{"spam.io"}) {
		t.Errorf("excluded_domains = %v", args["excluded_domains"])
	}
	if args["category"] != "news" {
		t.Errorf("category = %v", args["category"])
	}
	if args["start_published_date"] != "2024-01-01T00:00:00Z" {
		t.Errorf("start_published_date = %v", args["start_published_date"])
	}
	if args["end_published_date"] != "2024-12-31T00:00:00Z" {
		t.Errorf("end_published_date = %v", args["end_published_date"])
	}
	if args["max_age_hours"] != -1 {
		t.Errorf("max_age_hours = %v, want -1", args["max_age_hours"])
	}
}

func TestApplyToSearchArgs_RespectsModelOverridesForNewFields(t *testing.T) {
	zero := 0
	opts := webSearchOptions{
		searchContextSize:  "high",
		contentMode:        "text",
		maxContentChars:    9000,
		excludedDomains:    []string{"spam.io"},
		category:           "news",
		startPublishedDate: "2024-01-01T00:00:00Z",
		endPublishedDate:   "2024-12-31T00:00:00Z",
		maxAgeHours:        &zero,
	}
	args := map[string]any{
		"query":                "ai",
		"content_mode":         "highlights",
		"max_content_chars":    100,
		"excluded_domains":     []any{"other.example"},
		"category":             "research paper",
		"start_published_date": "2023-01-01T00:00:00Z",
		"end_published_date":   "2023-06-01T00:00:00Z",
		"max_age_hours":        99,
	}
	opts.applyToSearchArgs(args)

	if args["content_mode"] != "highlights" {
		t.Errorf("content_mode overridden, got %v", args["content_mode"])
	}
	if args["max_content_chars"] != 100 {
		t.Errorf("max_content_chars overridden, got %v", args["max_content_chars"])
	}
	if !reflect.DeepEqual(args["excluded_domains"], []any{"other.example"}) {
		t.Errorf("excluded_domains overridden, got %v", args["excluded_domains"])
	}
	if args["category"] != "research paper" {
		t.Errorf("category overridden, got %v", args["category"])
	}
	if args["start_published_date"] != "2023-01-01T00:00:00Z" {
		t.Errorf("start_published_date overridden, got %v", args["start_published_date"])
	}
	if args["end_published_date"] != "2023-06-01T00:00:00Z" {
		t.Errorf("end_published_date overridden, got %v", args["end_published_date"])
	}
	if args["max_age_hours"] != 99 {
		t.Errorf("max_age_hours overridden, got %v", args["max_age_hours"])
	}
}

func TestApplyToFetchArgs_ForwardsExcludedDomainsOnly(t *testing.T) {
	zero := 0
	opts := webSearchOptions{
		searchContextSize: "high",
		contentMode:       "text",
		maxContentChars:   2000,
		excludedDomains:   []string{"spam.io"},
		category:          "news",
		maxAgeHours:       &zero,
	}
	args := map[string]any{"urls": []any{"https://example.com/"}}
	opts.applyToFetchArgs(args)

	for _, k := range []string{"max_results", "content_mode", "max_content_chars", "category", "max_age_hours", "start_published_date", "end_published_date"} {
		if _, set := args[k]; set {
			t.Errorf("fetch args should not receive %s, got %v", k, args[k])
		}
	}
	if !reflect.DeepEqual(args["excluded_domains"], []any{"spam.io"}) {
		t.Errorf("excluded_domains = %v", args["excluded_domains"])
	}
}

func TestNormalizePublishedDate(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"2024-01-15", "2024-01-15T00:00:00Z"},
		{"2024-06-30T12:00:00Z", "2024-06-30T12:00:00Z"},
		{"2024-06-30T12:00:00+02:00", "2024-06-30T10:00:00Z"},
		{"not-a-date", ""},
		{"", ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := normalizePublishedDate(tc.in); got != tc.want {
			t.Errorf("normalizePublishedDate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIntValue_TriState(t *testing.T) {
	if n, ok := intValue(float64(0)); !ok || n != 0 {
		t.Errorf("intValue(0) = (%d, %v), want (0, true)", n, ok)
	}
	if n, ok := intValue(float64(-1)); !ok || n != -1 {
		t.Errorf("intValue(-1) = (%d, %v), want (-1, true)", n, ok)
	}
	if _, ok := intValue(nil); ok {
		t.Errorf("intValue(nil) should report not set")
	}
	if _, ok := intValue(""); ok {
		t.Errorf("intValue(\"\") should report not set")
	}
	if n, ok := intValue("42"); !ok || n != 42 {
		t.Errorf("intValue(\"42\") = (%d, %v), want (42, true)", n, ok)
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
