package toolruntime

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// webSearchOptions is the parsed view of the OpenAI web_search_options /
// Responses `web_search` tool-config surface that we forward to the MCP tools.
//
// piiCheck and injectionCheck capture the caller's opt-in for the websearch
// server's PII and prompt-injection filters. They are tri-state: a nil pointer
// means the caller did not send the control so the server should fall back to
// its own default; a non-nil pointer means the caller explicitly requested the
// filter on or off and the router forwards that decision as a header on the
// MCP session.
type webSearchOptions struct {
	userLocationCountry string
	allowedDomains      []string
	excludedDomains     []string
	searchContextSize   string
	contentMode         string
	maxContentChars     int
	category            string
	startPublishedDate  string
	endPublishedDate    string
	maxAgeHours         *int
	piiCheck            *bool
	injectionCheck      *bool
	// includeActionSources mirrors OpenAI's documented
	// `include: ["web_search_call.action.sources"]` request flag. When true,
	// the synthesized web_search_call output items carry an `action.sources`
	// array in the spec shape `[{type: "url", url: "..."}]`. When false
	// (the default) the field is omitted so spec-conformant clients are not
	// billed for data they did not ask for.
	includeActionSources bool
}

// normalizedContextSize returns search_context_size folded to its canonical
// lowercase/trimmed form so tier lookups stay consistent across helpers.
func (o webSearchOptions) normalizedContextSize() string {
	return strings.ToLower(strings.TrimSpace(o.searchContextSize))
}

// maxResults returns the per-search result cap derived from search_context_size.
// Zero means "let the MCP tool use its own default".
func (o webSearchOptions) maxResults() int {
	switch o.normalizedContextSize() {
	case "low":
		return searchContextResultsLow
	case "medium":
		return searchContextResultsMedium
	case "high":
		return searchContextResultsHigh
	default:
		return 0
	}
}

// tierContentMode returns the content_mode implied by the search_context_size
// tier. Low and medium favor query-relevant highlights to keep token use
// predictable; high asks for the full rendered page text. An empty string
// means "no tier preference, let the MCP server pick its default".
func (o webSearchOptions) tierContentMode() string {
	switch o.normalizedContextSize() {
	case "low", "medium":
		return contentModeHighlights
	case "high":
		return contentModeText
	default:
		return ""
	}
}

// tierMaxContentChars returns the per-result character budget implied by the
// tier. Zero means "no tier preference".
func (o webSearchOptions) tierMaxContentChars() int {
	switch o.normalizedContextSize() {
	case "low":
		return searchContextCharsLow
	case "medium":
		return searchContextCharsMedium
	case "high":
		return searchContextCharsHigh
	default:
		return 0
	}
}

// effectiveContentMode picks the caller's explicit content_mode when set and
// falls back to the tier-implied value otherwise.
func (o webSearchOptions) effectiveContentMode() string {
	if mode := strings.ToLower(strings.TrimSpace(o.contentMode)); mode != "" {
		return mode
	}
	return o.tierContentMode()
}

// effectiveMaxContentChars prefers an explicit positive caller override and
// falls back to the tier-implied budget.
func (o webSearchOptions) effectiveMaxContentChars() int {
	if o.maxContentChars > 0 {
		return o.maxContentChars
	}
	return o.tierMaxContentChars()
}

// applyToSearchArgs merges forwarded options into a `search` tool-call
// argument map produced by the model, leaving any model-supplied overrides in
// place so it can still narrow the request on its own.
func (o webSearchOptions) applyToSearchArgs(arguments map[string]any) {
	if arguments == nil {
		return
	}
	if _, set := arguments["max_results"]; !set {
		if n := o.maxResults(); n > 0 {
			arguments["max_results"] = n
		}
	}
	if _, set := arguments["content_mode"]; !set {
		if mode := o.effectiveContentMode(); mode != "" {
			arguments["content_mode"] = mode
		}
	}
	if _, set := arguments["max_content_chars"]; !set {
		if n := o.effectiveMaxContentChars(); n > 0 {
			arguments["max_content_chars"] = n
		}
	}
	if o.userLocationCountry != "" {
		if _, set := arguments["user_location_country"]; !set {
			arguments["user_location_country"] = o.userLocationCountry
		}
	}
	if len(o.allowedDomains) > 0 {
		if _, set := arguments["allowed_domains"]; !set {
			arguments["allowed_domains"] = toAnySlice(o.allowedDomains)
		}
	}
	if len(o.excludedDomains) > 0 {
		if _, set := arguments["excluded_domains"]; !set {
			arguments["excluded_domains"] = toAnySlice(o.excludedDomains)
		}
	}
	if o.category != "" {
		if _, set := arguments["category"]; !set {
			arguments["category"] = o.category
		}
	}
	if o.startPublishedDate != "" {
		if _, set := arguments["start_published_date"]; !set {
			arguments["start_published_date"] = o.startPublishedDate
		}
	}
	if o.endPublishedDate != "" {
		if _, set := arguments["end_published_date"]; !set {
			arguments["end_published_date"] = o.endPublishedDate
		}
	}
	if o.maxAgeHours != nil {
		if _, set := arguments["max_age_hours"]; !set {
			arguments["max_age_hours"] = *o.maxAgeHours
		}
	}
}

// applyToFetchArgs merges forwarded options into a `fetch` tool-call argument
// map. Retrieval-depth, content-mode, category, and date-range knobs are
// search-only so they are intentionally skipped here; only host-scoped filters
// are meaningful for a URL-targeted fetch.
func (o webSearchOptions) applyToFetchArgs(arguments map[string]any) {
	if arguments == nil {
		return
	}
	if len(o.allowedDomains) > 0 {
		if _, set := arguments["allowed_domains"]; !set {
			arguments["allowed_domains"] = toAnySlice(o.allowedDomains)
		}
	}
	if len(o.excludedDomains) > 0 {
		if _, set := arguments["excluded_domains"]; !set {
			arguments["excluded_domains"] = toAnySlice(o.excludedDomains)
		}
	}
}

// parseChatWebSearchOptions extracts OpenAI's documented web_search_options
// fields plus the sibling `filters` block from a Chat Completions request body.
// Returns the zero value when the block is missing so the caller can forward
// "no options" without special casing.
func parseChatWebSearchOptions(body map[string]any) webSearchOptions {
	opts := webSearchOptions{}
	if raw, ok := body["web_search_options"].(map[string]any); ok {
		opts.searchContextSize = stringValue(raw["search_context_size"])
		opts.userLocationCountry = extractUserLocationCountry(raw["user_location"])
		opts.allowedDomains = extractAllowedDomains(raw["filters"])
		opts.excludedDomains = extractExcludedDomains(raw["filters"])
		applyAdvancedSearchFields(&opts, raw)
	}
	if filters, ok := body["filters"].(map[string]any); ok {
		if len(opts.allowedDomains) == 0 {
			opts.allowedDomains = extractDomainListFromFilterMap(filters, "allowed_domains")
		}
		if len(opts.excludedDomains) == 0 {
			opts.excludedDomains = extractDomainListFromFilterMap(filters, "excluded_domains")
		}
	}
	opts.piiCheck = parseSafetyOptIn(body["pii_check_options"])
	opts.injectionCheck = parseSafetyOptIn(body["prompt_injection_check_options"])
	opts.includeActionSources = parseIncludeActionSources(body)
	return opts
}

// parseResponsesWebSearchOptions mirrors parseChatWebSearchOptions for the
// Responses API, where options live on the `tools[{type:"web_search", ...}]`
// entries rather than a sibling block.
func parseResponsesWebSearchOptions(body map[string]any) webSearchOptions {
	rawTools, _ := body["tools"].([]any)
	for _, rawTool := range rawTools {
		tool, _ := rawTool.(map[string]any)
		if tool == nil {
			continue
		}
		if stringValue(tool["type"]) != "web_search" {
			continue
		}
		opts := webSearchOptions{
			searchContextSize:   stringValue(tool["search_context_size"]),
			userLocationCountry: extractUserLocationCountry(tool["user_location"]),
		}
		opts.allowedDomains = extractAllowedDomains(tool["filters"])
		opts.excludedDomains = extractExcludedDomains(tool["filters"])
		applyAdvancedSearchFields(&opts, tool)
		opts.piiCheck = parseSafetyOptIn(body["pii_check_options"])
		opts.injectionCheck = parseSafetyOptIn(body["prompt_injection_check_options"])
		opts.includeActionSources = parseIncludeActionSources(body)
		return opts
	}
	return webSearchOptions{}
}

// parseIncludeActionSources reports whether the request opted into OpenAI's
// documented `web_search_call.action.sources` include flag. The flag lives
// at the top of the request body as `include: ["..."]` and unlocks the
// synthesized `action.sources` field on web_search_call output items. It
// defaults to false so we do not ship extra data to clients that did not
// ask for it.
func parseIncludeActionSources(body map[string]any) bool {
	raw, ok := body["include"].([]any)
	if !ok {
		return false
	}
	for _, item := range raw {
		if stringValue(item) == includeActionSourcesFlag {
			return true
		}
	}
	return false
}

// includeActionSourcesFlag is OpenAI's documented `ResponseIncludable`
// enum value that unlocks `web_search_call.action.sources`.
const includeActionSourcesFlag = "web_search_call.action.sources"

// stripRouterOwnedIncludes removes include entries that only the router
// knows how to service from the request body it forwards upstream.
// Upstream inference servers validate `include` against their own schema
// and reject unknown values, so the router must filter them out after
// reading them itself. An empty list is removed entirely to avoid
// shipping `"include":[]`, which some servers still reject.
func stripRouterOwnedIncludes(body map[string]any) {
	raw, ok := body["include"].([]any)
	if !ok {
		return
	}
	filtered := raw[:0]
	for _, item := range raw {
		if stringValue(item) == includeActionSourcesFlag {
			continue
		}
		filtered = append(filtered, item)
	}
	if len(filtered) == 0 {
		delete(body, "include")
		return
	}
	body["include"] = filtered
}

// parseSafetyOptIn reads a caller-provided safety control block such as
// `pii_check_options` or `prompt_injection_check_options` from a request body.
//
// The presence of the key is itself the signal that the caller wants the
// corresponding filter enabled for this request; the block's contents are
// currently ignored and reserved for future per-request tuning. Callers that
// omit the key get a nil pointer so the router forwards "no preference" and
// the websearch server falls back to its own default.
func parseSafetyOptIn(raw any) *bool {
	if raw == nil {
		return nil
	}
	enabled := true
	return &enabled
}

// safetyOptIns captures the caller's per-request opt-in for the websearch
// server's PII and prompt-injection filters so `connectToolSession` can turn
// them into `X-Tinfoil-Tool-*` headers on the MCP session. A nil pointer means
// the caller said nothing and the server should apply its own default.
type safetyOptIns struct {
	pii       *bool
	injection *bool
}

// parseSafetyOptIns pulls the caller-provided PII and prompt-injection opt-ins
// off the top-level request body. Both Chat Completions and Responses expose
// the controls as top-level keys (`pii_check_options`,
// `prompt_injection_check_options`) so a single parser covers both routes.
func parseSafetyOptIns(body map[string]any) safetyOptIns {
	return safetyOptIns{
		pii:       parseSafetyOptIn(body["pii_check_options"]),
		injection: parseSafetyOptIn(body["prompt_injection_check_options"]),
	}
}

// extractUserLocationCountry pulls the ISO 3166-1 alpha-2 country code out of
// OpenAI's user_location object, which nests the actual country under an
// `approximate` sub-object.
func extractUserLocationCountry(raw any) string {
	loc, _ := raw.(map[string]any)
	if len(loc) == 0 {
		return ""
	}
	if approx, ok := loc["approximate"].(map[string]any); ok {
		if country := strings.TrimSpace(stringValue(approx["country"])); country != "" {
			return strings.ToUpper(country)
		}
	}
	return strings.ToUpper(strings.TrimSpace(stringValue(loc["country"])))
}

// extractAllowedDomains reads filters.allowed_domains from either a
// web_search_options block or a Responses tool entry.
func extractAllowedDomains(raw any) []string {
	filters, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return extractDomainListFromFilterMap(filters, "allowed_domains")
}

// extractExcludedDomains mirrors extractAllowedDomains for the
// filters.excluded_domains list used to drop hosts from search results.
func extractExcludedDomains(raw any) []string {
	filters, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return extractDomainListFromFilterMap(filters, "excluded_domains")
}

// extractAllowedDomainsFromFilterMap is kept as a thin shim over the shared
// domain-list helper so existing callers keep compiling; new code should call
// extractDomainListFromFilterMap directly.
func extractAllowedDomainsFromFilterMap(filters map[string]any) []string {
	return extractDomainListFromFilterMap(filters, "allowed_domains")
}

// extractDomainListFromFilterMap reads an array of hostnames under `key` from
// a filters map and returns it lowercased, trimmed, and deduplicated.
func extractDomainListFromFilterMap(filters map[string]any, key string) []string {
	raw, ok := filters[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		domain := strings.ToLower(strings.TrimSpace(stringValue(item)))
		if domain == "" {
			continue
		}
		if _, dup := seen[domain]; dup {
			continue
		}
		seen[domain] = struct{}{}
		out = append(out, domain)
	}
	return out
}

// applyAdvancedSearchFields lifts the non-domain search knobs (content_mode,
// max_content_chars, category, publish date range, max_age_hours) off a
// caller-provided block into opts. Callers are responsible for the
// surrounding domain/country parsing so this stays focused on the fields that
// are identical across Chat and Responses surfaces.
func applyAdvancedSearchFields(opts *webSearchOptions, raw map[string]any) {
	if opts == nil || raw == nil {
		return
	}
	if mode := strings.TrimSpace(stringValue(raw["content_mode"])); mode != "" {
		opts.contentMode = mode
	}
	if n, ok := intValue(raw["max_content_chars"]); ok && n > 0 {
		opts.maxContentChars = n
	}
	if cat := strings.TrimSpace(stringValue(raw["category"])); cat != "" {
		opts.category = cat
	}
	if start := normalizePublishedDate(raw["start_published_date"]); start != "" {
		opts.startPublishedDate = start
	}
	if end := normalizePublishedDate(raw["end_published_date"]); end != "" {
		opts.endPublishedDate = end
	}
	if n, ok := intValue(raw["max_age_hours"]); ok {
		v := n
		opts.maxAgeHours = &v
	}
}

// intValue coerces a JSON-decoded number or numeric string into an int. The
// second return reports whether the input was a recognizable integer; callers
// can use that to distinguish "unset" from "explicitly zero", which matters
// for tri-state fields like max_age_hours. Fractional, NaN, and infinite
// floats are rejected rather than silently truncated so a caller that sends
// `1.5` does not have it quietly coerced to `1`.
func intValue(raw any) (int, bool) {
	switch v := raw.(type) {
	case nil:
		return 0, false
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		if v != math.Trunc(v) {
			return 0, false
		}
		if v < math.MinInt || v > math.MaxInt {
			return 0, false
		}
		return int(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		if n, err := strconv.Atoi(trimmed); err == nil {
			return n, true
		}
	}
	return 0, false
}

// normalizePublishedDate accepts `YYYY-MM-DD` or RFC 3339 strings and returns
// the value as RFC 3339 in UTC. Empty or malformed input is dropped rather
// than surfaced as an error so a single bad date does not fail the whole
// request; the MCP server still gets a syntactically valid value or nothing.
func normalizePublishedDate(raw any) string {
	str := strings.TrimSpace(stringValue(raw))
	if str == "" {
		return ""
	}
	if t, err := time.Parse("2006-01-02", str); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse(time.RFC3339, str); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

// applyWebSearchOptionsToToolCall dispatches to the per-tool merge helper so
// options forwarded from OpenAI's request shape end up on the MCP tool call.
func applyWebSearchOptionsToToolCall(toolName string, arguments map[string]any, opts webSearchOptions) {
	switch toolName {
	case "search":
		opts.applyToSearchArgs(arguments)
	case "fetch":
		opts.applyToFetchArgs(arguments)
	}
}

// toAnySlice lifts a []string into a []any so it marshals cleanly through the
// generic map[string]any request body the MCP client expects.
func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
