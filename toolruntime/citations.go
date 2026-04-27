package toolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type citationSource struct {
	index int
	url   string
	title string
}

type citationState struct {
	nextIndex int
	sources   []citationSource
	harmony   bool // when true, format tool output with cursors/line numbers and parse 【N†LX】 citations
}

// record registers a source the router surfaced to the model in a tool output
// so later passes over the model's final content can recognize inline markdown
// links pointing at that URL and emit url_citation annotations for them.
func (c *citationState) record(url, title string) int {
	if c == nil {
		return 1
	}
	if c.nextIndex <= 0 {
		c.nextIndex = 1
	}
	index := c.nextIndex
	c.nextIndex++
	c.sources = append(c.sources, citationSource{index: index, url: url, title: title})
	return index
}

// markdownLinkPattern matches inline markdown links of the form
// `[visible text](https://example.com)` so the router can map the rendered
// link span back to a recorded source URL. The label capture allows any
// characters except a closing bracket, matching the subset of markdown the
// model is asked to produce; nested brackets and images are not supported on
// purpose.
var markdownLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)

// fullwidthBracketedLinkPattern matches the near-markdown shape that some
// web-search tuned models emit when they slip from ASCII brackets into
// fullwidth lenticular brackets: `【visible text】(https://example.com)`.
// It also accepts the mixed-bracket variant `【visible text](https://...)`
// observed from gpt-oss when it improvises a citation outside the harmony
// browser tool training distribution. The router rewrites matches to
// canonical ASCII markdown before computing citation spans so every
// downstream renderer sees the documented shape.
var fullwidthBracketedLinkPattern = regexp.MustCompile(`\x{3010}([^\x{3011}\]]+)[\x{3011}\]]\((https?://[^\s)]+)\)`)

// normalizeCitationLinks rewrites `【label】(url)` (and the `【label](url)`
// mixed-bracket shape) occurrences in the model's final content into ASCII
// markdown `[label](url)` so every consumer renders a clickable link
// regardless of which bracket style the model produced.
func normalizeCitationLinks(text string) string {
	if text == "" || !strings.ContainsRune(text, '\u3010') {
		return text
	}
	return fullwidthBracketedLinkPattern.ReplaceAllString(text, "[$1]($2)")
}

// harmonyCitationPattern matches the Harmony citation format gpt-oss produces:
// 【cursor†Lstart-Lend】 or 【cursor†Lstart】. The cursor is the 1-based
// result number matching the [N] prefix in formatHarmonySearchToolOutput.
// The line references are captured but not used for URL resolution — the
// cursor alone identifies the source.
var harmonyCitationPattern = regexp.MustCompile(`\x{3010}(\d+)†L\d+(?:-L\d+)?\x{3011}`)

// resolveHarmonyCitations rewrites Harmony-format citations like 【3†L5-L8】
// into standard markdown links [title](url) by looking up the cursor number
// in the citationState's recorded sources. Unresolved cursors (no matching
// source) are left unchanged so the model's output is not silently corrupted.
func (c *citationState) resolveHarmonyCitations(text string) string {
	if c == nil || !c.harmony || !strings.ContainsRune(text, '\u3010') {
		return text
	}
	return harmonyCitationPattern.ReplaceAllStringFunc(text, func(match string) string {
		sub := harmonyCitationPattern.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		cursor := 0
		for _, ch := range sub[1] {
			cursor = cursor*10 + int(ch-'0')
		}
		for _, source := range c.sources {
			if source.index == cursor {
				title := source.title
				if title == "" {
					title = "Source"
				}
				return fmt.Sprintf("[%s](%s)", title, source.url)
			}
		}
		return match
	})
}

var toolOutputURLPattern = regexp.MustCompile(`(?m)^URL:\s*(\S+)`)

// extractToolOutputURLs pulls the `URL: ...` lines the router embeds into each
// numbered source block so the downstream stream emitter can surface them as
// web_search_call `sources`.
func extractToolOutputURLs(output string) []string {
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

// extractToolOutputSources pulls the `{Source, URL}` pairs the router
// embeds into each numbered result block so terminal tinfoil-event
// markers can attribute sources to the specific search call that
// produced them. Each block is formatted as:
//
//	Source: <title>
//	URL: <url>
//
// with the `Source:` line optional. Duplicate URLs are dropped so a
// single call never lists the same hit twice. Titles are best-effort;
// a missing `Source:` line yields an empty title.
func extractToolOutputSources(output string) []toolCallSource {
	if output == "" {
		return nil
	}
	lines := strings.Split(output, "\n")
	sources := make([]toolCallSource, 0)
	seen := make(map[string]struct{})
	var pendingTitle string
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "Source:") {
			pendingTitle = strings.TrimSpace(strings.TrimPrefix(trimmed, "Source:"))
			continue
		}
		if strings.HasPrefix(trimmed, "URL:") {
			url := strings.TrimSpace(strings.TrimPrefix(trimmed, "URL:"))
			if url == "" {
				pendingTitle = ""
				continue
			}
			if _, dup := seen[url]; dup {
				pendingTitle = ""
				continue
			}
			seen[url] = struct{}{}
			sources = append(sources, toolCallSource{url: url, title: pendingTitle})
			pendingTitle = ""
		}
	}
	if len(sources) == 0 {
		return nil
	}
	return sources
}

type annotationMatch struct {
	// startIndex and endIndex span the visible label of the markdown link
	// (i.e. the text between the square brackets), matching the shape of
	// OpenAI's url_citation start_index/end_index fields.
	startIndex int
	endIndex   int
	source     citationSource
}

// matchesFor scans the model's final content for inline markdown links whose
// URL matches a source the router recorded during tool execution and returns
// one annotationMatch per such occurrence. The start/end indices span the
// link's visible label (the characters between the square brackets), which is
// the convention OpenAI's web_search_call annotations use.
//
// Links that point at URLs the router never recorded are ignored so a model
// that fabricates a URL cannot cause a broken annotation to ship. A given
// label+URL pair can appear multiple times in the text; each occurrence
// produces its own annotation to match how OpenAI's Responses API reports
// repeated citations.
func (c *citationState) matchesFor(text string) []annotationMatch {
	if c == nil || len(c.sources) == 0 || text == "" {
		return nil
	}

	// byURL is keyed by the normalized URL so cosmetic differences
	// between the URL the search tool recorded and the URL the model
	// re-emitted (trailing slash, www prefix, tracking params,
	// invisible runes, etc.) do not cause the lookup to miss.
	byURL := make(map[string]citationSource, len(c.sources))
	for _, source := range c.sources {
		if source.url == "" {
			continue
		}
		key := normalizeCitationURL(source.url)
		if key == "" {
			continue
		}
		if existing, ok := byURL[key]; !ok || (existing.title == "" && source.title != "") {
			byURL[key] = source
		}
	}
	if len(byURL) == 0 {
		return nil
	}

	raw := markdownLinkPattern.FindAllStringSubmatchIndex(text, -1)
	if len(raw) == 0 {
		return nil
	}

	byteToRune := newByteToRuneIndex(text)
	matches := make([]annotationMatch, 0, len(raw))
	for _, m := range raw {
		if len(m) < 6 {
			continue
		}
		url := text[m[4]:m[5]]
		source, ok := byURL[normalizeCitationURL(url)]
		if !ok {
			continue
		}
		source.url = url // Use the model's URL so the webapp can match it against the rendered markdown link
		// Span the visible label, not the whole [label](url) expression, so
		// downstream consumers can highlight just the link text the user sees.
		// Convert regex byte offsets to Unicode code-point offsets so the
		// indices match what OpenAI SDKs and JS/Python clients observe when
		// they index strings by character.
		matches = append(matches, annotationMatch{
			startIndex: byteToRune.convert(m[2]),
			endIndex:   byteToRune.convert(m[3]),
			source:     source,
		})
	}
	return matches
}

// byteToRuneIndex translates byte offsets into rune (code point) offsets for
// a given string. It caches the last lookup so sequential calls, as produced
// by a left-to-right scan of regex matches, stay linear in the source length.
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

// nestedAnnotationsFor returns annotations in the Chat Completions API shape
// documented by OpenAI:
//
//	{"type":"url_citation","url_citation":{"title":..,"url":..,"start_index":..,"end_index":..}}
func (c *citationState) nestedAnnotationsFor(text string) []any {
	matches := c.matchesFor(text)
	if len(matches) == 0 {
		return nil
	}
	annotations := make([]any, 0, len(matches))
	for _, match := range matches {
		citation := map[string]any{
			"url":         match.source.url,
			"start_index": match.startIndex,
			"end_index":   match.endIndex,
		}
		if match.source.title != "" {
			citation["title"] = match.source.title
		}
		annotations = append(annotations, map[string]any{
			"type":         "url_citation",
			"url_citation": citation,
		})
	}
	return annotations
}

// flatAnnotationsFor returns annotations in the Responses API shape documented
// by OpenAI, where url_citation fields live directly on the annotation object:
//
//	{"type":"url_citation","start_index":..,"end_index":..,"url":..,"title":..}
func (c *citationState) flatAnnotationsFor(text string) []any {
	matches := c.matchesFor(text)
	if len(matches) == 0 {
		return nil
	}
	annotations := make([]any, 0, len(matches))
	for _, match := range matches {
		annotation := map[string]any{
			"type":        "url_citation",
			"start_index": match.startIndex,
			"end_index":   match.endIndex,
			"url":         match.source.url,
		}
		if match.source.title != "" {
			annotation["title"] = match.source.title
		}
		annotations = append(annotations, annotation)
	}
	return annotations
}

// publicToolErrorReason returns a short, opaque status string safe to
// ship to clients via `web_search_call.reason`. The raw error text is
// recorded to the server log so operators can still diagnose failures
// without having to surface internal hostnames, gRPC error bodies, or
// other implementation details to end users.
//
// Safety-blocked errors (PII or prompt-injection safeguards tripping on the
// caller's query) return the distinct `blockedToolErrorReason` so the
// caller can render them with the dedicated `blocked` web_search_call
// status instead of collapsing them into a generic failure.
const (
	publicToolErrorReasonString = "tool_error"
	blockedToolErrorReason      = "blocked_by_safety_filter"
)

func publicToolErrorReason(toolName string, err error) string {
	if err == nil {
		return ""
	}
	debugLogf("toolruntime: %s tool call failed: %v", toolName, err)
	if isToolCallBlocked(err) {
		return blockedToolErrorReason
	}
	return publicToolErrorReasonString
}

// isToolCallBlocked reports whether the MCP tool error came from a PII or
// prompt-injection safeguard. Safety-blocked tool errors are wrapped with
// a message beginning `query was blocked by safety filters`. Detecting
// that prefix lets the router surface `status: "blocked"` on the
// corresponding web_search_call envelope so clients can render a distinct
// affordance without exposing the raw detail string.
func isToolCallBlocked(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "blocked by safety filter")
}

// failureStatusFor picks the web_search_call status string to surface for
// a non-nil MCP error. Safety-blocked errors become `"blocked"` so clients
// can distinguish them from generic tool failures; everything else stays
// `"failed"`.
func failureStatusFor(err error) string {
	if isToolCallBlocked(err) {
		return "blocked"
	}
	return "failed"
}

// toolResultErrorMessage extracts a human-readable error message from a
// CallToolResult whose `IsError` flag is set. The MCP SDK packs handler
// errors into the result's text content rather than returning them as
// protocol errors, so callers need this helper to surface the underlying
// message (e.g. "query was blocked by safety filters: ...") to the rest
// of the router for classification and user-visible reporting.
func toolResultErrorMessage(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var parts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			if trimmed := strings.TrimSpace(textContent.Text); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func callTool(ctx context.Context, session *mcp.ClientSession, name string, arguments map[string]any, citations *citationState) (string, error) {
	start := time.Now()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		debugLogf("toolruntime:mcp.error tool=%s elapsed=%s args=%s err=%v", name, time.Since(start), debugPreview(arguments, 400), err)
		return "", err
	}
	if result.IsError {
		message := toolResultErrorMessage(result)
		debugLogf("toolruntime:mcp.tool_error tool=%s elapsed=%s args=%s message=%q", name, time.Since(start), debugPreview(arguments, 400), message)
		if message == "" {
			message = "tool call failed"
		}
		return "", errors.New(message)
	}
	if debugEnabled {
		hasStructured := result.StructuredContent != nil
		textParts := 0
		for _, content := range result.Content {
			if _, ok := content.(*mcp.TextContent); ok {
				textParts++
			}
		}
		debugLogf("toolruntime:mcp.ok tool=%s elapsed=%s structured=%t text_parts=%d structured_preview=%s",
			name, time.Since(start), hasStructured, textParts, debugPreview(result.StructuredContent, 500))
	}
	if result.StructuredContent != nil {
		if formatted := formatStructuredToolOutput(name, result.StructuredContent, citations); formatted != "" {
			return formatted, nil
		}
		body, err := json.Marshal(result.StructuredContent)
		if err == nil {
			return string(body), nil
		}
	}
	var parts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, textContent.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func formatStructuredToolOutput(name string, raw any, citations *citationState) string {
	content, _ := raw.(map[string]any)
	if len(content) == 0 {
		return ""
	}

	switch {
	case isRouterSearchToolName(name):
		return formatSearchToolOutput(content["results"], citations)
	case isRouterFetchToolName(name):
		if formatted := formatFetchToolOutput(content["pages"], citations); formatted != "" {
			return formatted
		}
		return formatFetchFailures(content["results"])
	default:
		return ""
	}
}

func formatSearchToolOutput(raw any, citations *citationState) string {
	results, _ := raw.([]any)
	if len(results) == 0 {
		return "No safe search results were found."
	}
	if citations != nil && citations.harmony {
		return formatHarmonySearchToolOutput(results, citations)
	}

	var out strings.Builder
	for _, rawResult := range results {
		result, _ := rawResult.(map[string]any)
		if result == nil {
			continue
		}

		title := strings.TrimSpace(stringValue(result["title"]))
		url := strings.TrimSpace(stringValue(result["url"]))
		content := strings.TrimSpace(stringValue(result["content"]))
		published := strings.TrimSpace(stringValue(result["published_date"]))
		citations.record(url, title)

		if title == "" {
			title = "Search result"
		}
		fmt.Fprintf(&out, "Source: %s\n", title)
		if url != "" {
			fmt.Fprintf(&out, "URL: %s\n", url)
		}
		if published != "" {
			fmt.Fprintf(&out, "Published: %s\n", published)
		}
		if content != "" {
			out.WriteString(content)
			out.WriteString("\n")
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String())
}

// formatHarmonySearchToolOutput formats search results with numbered cursors
// and line numbers so gpt-oss can cite them using its trained Harmony format:
// 【cursor†Lstart-Lend】.
func formatHarmonySearchToolOutput(results []any, citations *citationState) string {
	var out strings.Builder
	for _, rawResult := range results {
		result, _ := rawResult.(map[string]any)
		if result == nil {
			continue
		}

		title := strings.TrimSpace(stringValue(result["title"]))
		url := strings.TrimSpace(stringValue(result["url"]))
		content := strings.TrimSpace(stringValue(result["content"]))
		published := strings.TrimSpace(stringValue(result["published_date"]))
		cursor := citations.record(url, title)

		if title == "" {
			title = "Search result"
		}
		fmt.Fprintf(&out, "[%d] %s\n", cursor, title)
		if url != "" {
			fmt.Fprintf(&out, "URL: %s\n", url)
		}
		if published != "" {
			fmt.Fprintf(&out, "Published: %s\n", published)
		}
		if content != "" {
			lines := strings.Split(content, "\n")
			for i, line := range lines {
				fmt.Fprintf(&out, "L%d: %s\n", i+1, line)
			}
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String())
}

func formatFetchToolOutput(raw any, citations *citationState) string {
	pages, _ := raw.([]any)
	if len(pages) == 0 {
		return ""
	}
	if citations != nil && citations.harmony {
		return formatHarmonyFetchToolOutput(pages, citations)
	}

	var out strings.Builder
	for _, rawPage := range pages {
		page, _ := rawPage.(map[string]any)
		if page == nil {
			continue
		}

		url := strings.TrimSpace(stringValue(page["url"]))
		content := strings.TrimSpace(stringValue(page["content"]))
		citations.record(url, "Fetched page")
		out.WriteString("Source: Fetched page\n")
		if url != "" {
			fmt.Fprintf(&out, "URL: %s\n", url)
		}
		if content != "" {
			out.WriteString(content)
			out.WriteString("\n")
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String())
}

func formatHarmonyFetchToolOutput(pages []any, citations *citationState) string {
	var out strings.Builder
	for _, rawPage := range pages {
		page, _ := rawPage.(map[string]any)
		if page == nil {
			continue
		}

		url := strings.TrimSpace(stringValue(page["url"]))
		content := strings.TrimSpace(stringValue(page["content"]))
		cursor := citations.record(url, "Fetched page")

		fmt.Fprintf(&out, "[%d] Fetched page\n", cursor)
		if url != "" {
			fmt.Fprintf(&out, "URL: %s\n", url)
		}
		if content != "" {
			lines := strings.Split(content, "\n")
			for i, line := range lines {
				fmt.Fprintf(&out, "L%d: %s\n", i+1, line)
			}
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String())
}

func formatFetchFailures(raw any) string {
	results, _ := raw.([]any)
	if len(results) == 0 {
		return ""
	}

	lines := make([]string, 0, len(results))
	for _, rawResult := range results {
		result, _ := rawResult.(map[string]any)
		if result == nil {
			continue
		}
		status := strings.TrimSpace(stringValue(result["status"]))
		if status == "completed" {
			continue
		}
		url := strings.TrimSpace(stringValue(result["url"]))
		errText := strings.TrimSpace(stringValue(result["error"]))
		switch {
		case url != "" && errText != "":
			lines = append(lines, fmt.Sprintf("Fetch failed for %s: %s", url, errText))
		case url != "":
			lines = append(lines, fmt.Sprintf("Fetch failed for %s.", url))
		case errText != "":
			lines = append(lines, fmt.Sprintf("Fetch failed: %s", errText))
		}
	}
	return strings.Join(lines, "\n")
}

// shiftNestedAnnotationIndices shifts every `start_index` / `end_index`
// on a nested-shape url_citation by `offset` bytes. Called after marker
// injection to keep the annotation spans pointing at the same substrings
// of the (now longer) content string.
func shiftNestedAnnotationIndices(annotations []any, offset int) {
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

// shiftFlatAnnotationIndices is the Responses-shape counterpart of
// shiftNestedAnnotationIndices: it mutates every `start_index` /
// `end_index` on a flat url_citation annotation list so the spans
// continue to reference the same substrings after a marker prefix is
// injected into the output_text content.
func shiftFlatAnnotationIndices(annotations []any, offset int) {
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
