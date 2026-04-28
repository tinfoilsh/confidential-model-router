package citations

import (
	"fmt"
	"strings"
)

// Instructions is the system prompt text that tells the model how to format
// inline citations as markdown links. The citations package parses the
// links the model produces in response.
const Instructions = "Attach sources by embedding a clickable markdown link to the original URL directly after the sentence it supports. Format every citation exactly like this example, copying the punctuation characters verbatim: The sky is blue [Example page](https://example.com/article). Reference 1-2 sources per claim; do not reference every source on every sentence. Copy the URL character-for-character from the tool output: preserve or omit a trailing slash exactly as the tool emitted it, keep query parameters verbatim, and do not append punctuation, whitespace, zero-width characters, or any other character after the URL before the closing parenthesis. Never invent URLs, never paraphrase URLs, and never wrap the link in any other brackets, braces, or quotation marks."

// HarmonyInstructions is the system prompt text for Harmony-mode citation
// format (gpt-oss). The model cites using 【cursor†Lstart-Lend】 tokens
// which ResolveHarmonyCitations rewrites into markdown links.
const HarmonyInstructions = "Each search result is numbered with a cursor like [1], [2], etc. and its content is split into lines. When you cite a source, use the Harmony citation format: 【cursor†Lstart-Lend】 where cursor is the result number and Lstart-Lend is the line range. For example, 【3†L5-L8】 cites lines 5 through 8 of result 3. For a single line, use 【2†L4】. Reference 1-2 sources per claim. Never invent citations."

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// FormatSearchOutput formats search results into the text payload the
// model sees. When the State has Harmony mode enabled, it delegates to
// the harmony-specific formatter with numbered cursors and line numbers.
func FormatSearchOutput(raw any, state *State) string {
	results, _ := raw.([]any)
	if len(results) == 0 {
		return "No safe search results were found."
	}
	if state != nil && state.Harmony {
		return formatHarmonySearchOutput(results, state)
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
		if state != nil {
			state.Record(url, title)
		}

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

// formatHarmonySearchOutput formats search results with numbered cursors
// and line numbers so gpt-oss can cite them using its trained Harmony format:
// 【cursor†Lstart-Lend】.
func formatHarmonySearchOutput(results []any, state *State) string {
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
		cursor := state.Record(url, title)

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

// FormatFetchOutput formats fetched page results into the text payload
// the model sees. Returns empty string when there are no pages.
func FormatFetchOutput(raw any, state *State) string {
	pages, _ := raw.([]any)
	if len(pages) == 0 {
		return ""
	}
	if state != nil && state.Harmony {
		return formatHarmonyFetchOutput(pages, state)
	}

	var out strings.Builder
	for _, rawPage := range pages {
		page, _ := rawPage.(map[string]any)
		if page == nil {
			continue
		}

		url := strings.TrimSpace(stringValue(page["url"]))
		content := strings.TrimSpace(stringValue(page["content"]))
		if state != nil {
			state.Record(url, "Fetched page")
		}
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

func formatHarmonyFetchOutput(pages []any, state *State) string {
	var out strings.Builder
	for _, rawPage := range pages {
		page, _ := rawPage.(map[string]any)
		if page == nil {
			continue
		}

		url := strings.TrimSpace(stringValue(page["url"]))
		content := strings.TrimSpace(stringValue(page["content"]))
		cursor := state.Record(url, "Fetched page")

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

// FormatFetchFailures formats fetch failure results into an error message.
func FormatFetchFailures(raw any) string {
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
