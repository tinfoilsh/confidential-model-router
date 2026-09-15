package toolruntime

import (
	"strings"
	"unicode/utf8"
)

const (
	maxSourceSnippetBytes   = 1500
	maxMarkerSnippetBytes   = 6000
	snippetTruncationNotice = "\n[Excerpt truncated]"
)

func boundedSourceSnippet(text string, budget int) string {
	limit := min(maxSourceSnippetBytes, budget)
	if len(text) <= limit {
		return text
	}
	if limit <= len(snippetTruncationNotice) {
		return ""
	}
	end := limit - len(snippetTruncationNotice)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + snippetTruncationNotice
}

func structuredFetchToolCallSources(name string, structured any) []toolCallSource {
	if !isRouterFetchToolName(name) {
		return nil
	}
	content, _ := structured.(map[string]any)
	pages, _ := content["pages"].([]any)
	var sources []toolCallSource
	for _, raw := range pages {
		page, _ := raw.(map[string]any)
		url := strings.TrimSpace(stringValue(page["url"]))
		if url == "" {
			continue
		}
		sources = append(sources, toolCallSource{
			url:     url,
			title:   "Fetched page",
			snippet: strings.TrimSpace(stringValue(page["content"])),
		})
	}
	return sources
}

func sourcesForURL(sources []toolCallSource, url string) []toolCallSource {
	var matches []toolCallSource
	for _, source := range sources {
		if source.url == url {
			matches = append(matches, source)
		}
	}
	return matches
}
