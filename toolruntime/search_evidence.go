package toolruntime

import "strings"

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
			snippet: stringValue(page["content"]),
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
