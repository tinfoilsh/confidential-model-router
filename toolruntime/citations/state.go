package citations

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// --- Types, recording, and extraction ---

// Source tracks one URL the router surfaced to the model in a tool output.
// Index is the 1-based cursor number assigned when the source was recorded.
type Source struct {
	Index int
	URL   string
	Title string
}

// State accumulates the sources the router surfaced during tool execution
// so later passes over the model's final content can recognize inline
// markdown links and emit url_citation annotations.
type State struct {
	NextIndex int
	Sources   []Source
	Harmony   bool // when true, format tool output with cursors/line numbers and parse 【N†LX】 citations
}

// Record registers a source the router surfaced to the model in a tool output
// and returns the assigned 1-based cursor number.
func (c *State) Record(url, title string) int {
	if c == nil {
		return 1
	}
	if c.NextIndex <= 0 {
		c.NextIndex = 1
	}
	index := c.NextIndex
	c.NextIndex++
	c.Sources = append(c.Sources, Source{Index: index, URL: url, Title: title})
	return index
}

// ResolveHarmonyCitations rewrites Harmony-format citations like 【3†L5-L8】
// or bare 【3】 into standard markdown links [title](url) by looking up the
// cursor number in the State's recorded sources. Unresolved cursors (no
// matching source) are left unchanged so the model's output is not silently
// corrupted.
func (c *State) ResolveHarmonyCitations(text string) string {
	if c == nil || !c.Harmony || !strings.ContainsRune(text, '\u3010') {
		return text
	}
	return harmonyCitationPattern.ReplaceAllStringFunc(text, func(match string) string {
		sub := harmonyCitationPattern.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		cursor, err := strconv.Atoi(sub[1])
		if err != nil {
			return match
		}
		for _, source := range c.Sources {
			if source.Index == cursor {
				title := source.Title
				if title == "" {
					title = "Source"
				}
				return fmt.Sprintf("[%s](%s)", title, source.URL)
			}
		}
		return match
	})
}

// ToolOutputSource is a {url, title} pair extracted from router-formatted
// tool output. This is separate from Source (which carries an Index for
// cursor-based citation resolution) because extraction happens after
// formatting and the cursor is not needed at the extraction site.
type ToolOutputSource struct {
	URL   string
	Title string
}

var toolOutputURLPattern = regexp.MustCompile(`(?m)^URL:\s*(\S+)`)

// ExtractToolOutputURLs pulls the `URL: ...` lines the router embeds into each
// numbered source block so the downstream stream emitter can surface them as
// web_search_call `sources`.
func ExtractToolOutputURLs(output string) []string {
	matches := toolOutputURLPattern.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return nil
	}
	urls := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		url := strings.TrimSpace(match[1])
		if url == "" {
			continue
		}
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		urls = append(urls, url)
	}
	return urls
}

// ExtractToolOutputSources pulls the `{Source, URL}` pairs the router
// embeds into each numbered result block so terminal tinfoil-event
// markers can attribute sources to the specific search call that
// produced them.
func ExtractToolOutputSources(output string) []ToolOutputSource {
	if output == "" {
		return nil
	}
	lines := strings.Split(output, "\n")
	sources := make([]ToolOutputSource, 0)
	seen := make(map[string]struct{})
	var pendingTitle string
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if after, ok := strings.CutPrefix(trimmed, "Source:"); ok {
			pendingTitle = strings.TrimSpace(after)
			continue
		}
		if after, ok := strings.CutPrefix(trimmed, "URL:"); ok {
			url := strings.TrimSpace(after)
			if url == "" {
				pendingTitle = ""
				continue
			}
			if _, dup := seen[url]; dup {
				pendingTitle = ""
				continue
			}
			seen[url] = struct{}{}
			sources = append(sources, ToolOutputSource{URL: url, Title: pendingTitle})
			pendingTitle = ""
		}
	}
	if len(sources) == 0 {
		return nil
	}
	return sources
}

// --- Link matching and annotations ---

// MarkdownLinkPattern matches inline markdown links of the form
// `[visible text](https://example.com)` so the router can map the rendered
// link span back to a recorded source URL.
var MarkdownLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)

// fullwidthBracketedLinkPattern matches the near-markdown shape that some
// web-search tuned models emit when they slip from ASCII brackets into
// fullwidth lenticular brackets: `【visible text】(https://example.com)`.
// It also accepts the mixed-bracket variant `【visible text](https://...)`
// observed from gpt-oss when it improvises a citation outside the harmony
// browser tool training distribution.
var fullwidthBracketedLinkPattern = regexp.MustCompile(`\x{3010}([^\x{3011}\]]+)[\x{3011}\]]\((https?://[^\s)]+)\)`)

// harmonyCitationPattern matches the Harmony citation format gpt-oss produces:
// 【cursor†Lstart-Lend】, 【cursor†Lstart】, or bare 【cursor】. The cursor
// is the 1-based result number matching the [N] prefix in
// FormatHarmonySearchOutput. The line-range suffix is optional because the
// model sometimes drops it (e.g. 【1】 instead of 【1†L1-L3】) and the
// router only needs the cursor number to resolve the source.
var harmonyCitationPattern = regexp.MustCompile(`\x{3010}(\d+)(?:†L\d+(?:-L\d+)?)?\x{3011}`)

// NormalizeLinks rewrites `【label】(url)` (and the `【label](url)`
// mixed-bracket shape) occurrences in the model's final content into ASCII
// markdown `[label](url)` so every consumer renders a clickable link
// regardless of which bracket style the model produced.
func NormalizeLinks(text string) string {
	if text == "" || !strings.ContainsRune(text, '\u3010') {
		return text
	}
	return fullwidthBracketedLinkPattern.ReplaceAllString(text, "[$1]($2)")
}

// AnnotationMatch records a single inline markdown link whose URL matched a
// recorded source. StartIndex and EndIndex span the visible label of the
// markdown link (the text between square brackets), in rune (code point)
// offsets matching OpenAI's url_citation start_index/end_index fields.
type AnnotationMatch struct {
	StartIndex int
	EndIndex   int
	Source     Source
}

// MatchesFor scans the model's final content for inline markdown links whose
// URL matches a source the router recorded during tool execution and returns
// one AnnotationMatch per such occurrence.
func (c *State) MatchesFor(text string) []AnnotationMatch {
	if c == nil || len(c.Sources) == 0 || text == "" {
		return nil
	}

	byURL := make(map[string]Source, len(c.Sources))
	for _, source := range c.Sources {
		if source.URL == "" {
			continue
		}
		key := NormalizeURL(source.URL)
		if key == "" {
			continue
		}
		if existing, ok := byURL[key]; !ok || (existing.Title == "" && source.Title != "") {
			byURL[key] = source
		}
	}
	if len(byURL) == 0 {
		return nil
	}

	raw := MarkdownLinkPattern.FindAllStringSubmatchIndex(text, -1)
	if len(raw) == 0 {
		return nil
	}

	byteToRune := newByteToRuneIndex(text)
	matches := make([]AnnotationMatch, 0, len(raw))
	for _, m := range raw {
		if len(m) < 6 {
			continue
		}
		url := text[m[4]:m[5]]
		source, ok := byURL[NormalizeURL(url)]
		if !ok {
			continue
		}
		source.URL = url // Use the model's URL so the webapp can match it against the rendered markdown link
		matches = append(matches, AnnotationMatch{
			StartIndex: byteToRune.convert(m[2]),
			EndIndex:   byteToRune.convert(m[3]),
			Source:     source,
		})
	}
	return matches
}

// ResolveSource returns the most informative recorded source for the given
// URL, or (zero, false) when the URL was never registered during the tool
// loop. The URL comparison is done on the normalized form.
func (c *State) ResolveSource(url string) (Source, bool) {
	if c == nil || url == "" {
		return Source{}, false
	}
	target := NormalizeURL(url)
	if target == "" {
		return Source{}, false
	}
	var best Source
	found := false
	for _, source := range c.Sources {
		if NormalizeURL(source.URL) != target {
			continue
		}
		if !found || (best.Title == "" && source.Title != "") {
			best = source
			found = true
		}
	}
	return best, found
}

// NestedAnnotationsFor returns annotations in the Chat Completions API shape:
//
//	{"type":"url_citation","url_citation":{"title":..,"url":..,"start_index":..,"end_index":..}}
func (c *State) NestedAnnotationsFor(text string) []any {
	matches := c.MatchesFor(text)
	if len(matches) == 0 {
		return nil
	}
	annotations := make([]any, 0, len(matches))
	for _, match := range matches {
		citation := map[string]any{
			"url":         match.Source.URL,
			"start_index": match.StartIndex,
			"end_index":   match.EndIndex,
		}
		if match.Source.Title != "" {
			citation["title"] = match.Source.Title
		}
		annotations = append(annotations, map[string]any{
			"type":         "url_citation",
			"url_citation": citation,
		})
	}
	return annotations
}

// FlatAnnotationsFor returns annotations in the Responses API shape:
//
//	{"type":"url_citation","start_index":..,"end_index":..,"url":..,"title":..}
func (c *State) FlatAnnotationsFor(text string) []any {
	matches := c.MatchesFor(text)
	if len(matches) == 0 {
		return nil
	}
	annotations := make([]any, 0, len(matches))
	for _, match := range matches {
		annotation := map[string]any{
			"type":        "url_citation",
			"start_index": match.StartIndex,
			"end_index":   match.EndIndex,
			"url":         match.Source.URL,
		}
		if match.Source.Title != "" {
			annotation["title"] = match.Source.Title
		}
		annotations = append(annotations, annotation)
	}
	return annotations
}

// ShiftNestedAnnotationIndices shifts every `start_index` / `end_index`
// on a nested-shape url_citation by `offset` runes.
func ShiftNestedAnnotationIndices(annotations []any, offset int) {
	if offset == 0 {
		return
	}
	for _, raw := range annotations {
		ann, _ := raw.(map[string]any)
		if ann == nil {
			continue
		}
		inner, _ := ann["url_citation"].(map[string]any)
		if inner == nil {
			continue
		}
		if start, ok := inner["start_index"].(int); ok {
			inner["start_index"] = start + offset
		}
		if end, ok := inner["end_index"].(int); ok {
			inner["end_index"] = end + offset
		}
	}
}

// ShiftFlatAnnotationIndices is the Responses-shape counterpart of
// ShiftNestedAnnotationIndices.
func ShiftFlatAnnotationIndices(annotations []any, offset int) {
	if offset == 0 {
		return
	}
	for _, raw := range annotations {
		ann, _ := raw.(map[string]any)
		if ann == nil {
			continue
		}
		if start, ok := ann["start_index"].(int); ok {
			ann["start_index"] = start + offset
		}
		if end, ok := ann["end_index"].(int); ok {
			ann["end_index"] = end + offset
		}
	}
}

// byteToRuneIndex translates byte offsets into rune (code point) offsets for
// a given string. It caches the last lookup so sequential calls stay linear.
type byteToRuneIndex struct {
	text     string
	lastByte int
	lastRune int
}

func newByteToRuneIndex(text string) *byteToRuneIndex {
	return &byteToRuneIndex{text: text}
}

func (b *byteToRuneIndex) convert(byteOffset int) int {
	if byteOffset <= b.lastByte {
		b.lastByte = 0
		b.lastRune = 0
	}
	for b.lastByte < byteOffset && b.lastByte < len(b.text) {
		_, size := utf8.DecodeRuneInString(b.text[b.lastByte:])
		if size == 0 {
			break
		}
		b.lastByte += size
		b.lastRune++
	}
	return b.lastRune
}

// --- URL normalization ---

// citationTrackingParams lists the query-string keys whose presence or
// absence does not change which resource a URL identifies. They are
// dropped from the URL before comparison so a citation that carries
// `?utm_source=exa` still matches the canonical copy returned by the
// search tool.
var citationTrackingParams = map[string]struct{}{
	"utm_source":   {},
	"utm_medium":   {},
	"utm_campaign": {},
	"utm_term":     {},
	"utm_content":  {},
	"utm_id":       {},
	"fbclid":       {},
	"gclid":        {},
	"mc_cid":       {},
	"mc_eid":       {},
	"ref":          {},
	"ref_src":      {},
}

// NormalizeURL reduces a URL to a canonical form used as the
// citation-match key. The returned string identifies the same resource
// across the cosmetic differences that real-world search output and
// model-emitted markdown disagree on: trailing slashes, `www.` host
// prefixes, scheme/host casing, tracking query params, fragments, and
// invisible Unicode characters that tokenizers sometimes smuggle onto
// URL tails.
//
// The original URL string is never mutated; only the match key is
// normalized. Annotations still ship the URL the search tool returned
// so downstream renderers continue to receive the canonical form the
// source advertised.
//
// Inputs that do not parse as URLs are returned unchanged so the lookup
// falls back to exact-string equality for those pathological cases
// instead of silently collapsing distinct strings.
func NormalizeURL(raw string) string {
	raw = stripInvisibleRunes(raw)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Host = strings.TrimPrefix(parsed.Host, "www.")

	if len(parsed.Path) > 1 {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}

	if query := parsed.Query(); len(query) > 0 {
		for key := range query {
			if _, drop := citationTrackingParams[strings.ToLower(key)]; drop {
				query.Del(key)
			}
		}
		parsed.RawQuery = query.Encode()
	}

	parsed.Fragment = ""
	parsed.RawFragment = ""

	return parsed.String()
}

// stripInvisibleRunes removes Unicode format and zero-width characters
// that models occasionally emit at URL boundaries. These are visually
// absent but make two otherwise identical strings compare unequal under
// byte or rune equality.
func stripInvisibleRunes(raw string) string {
	if raw == "" {
		return raw
	}
	for _, r := range raw {
		if shouldStripRune(r) {
			goto rebuild
		}
	}
	return raw

rebuild:
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if shouldStripRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func shouldStripRune(r rune) bool {
	switch r {
	case '\u200B', '\u200C', '\u200D', '\u200E', '\u200F',
		'\u2028', '\u2029', '\u202A', '\u202B', '\u202C', '\u202D', '\u202E',
		'\u2060', '\u2061', '\u2062', '\u2063', '\u2064',
		'\uFEFF':
		return true
	}
	return unicode.Is(unicode.Cf, r)
}
