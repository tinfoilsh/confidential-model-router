package citations

import (
	"fmt"
	"strings"
)

// StringValue extracts a string from an any-typed value, returning ""
// if the value is not a string. Exported because callers in toolruntime
// already have their own stringValue but the format functions here need
// the same helper.
func StringValue(v any) string {
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

		title := strings.TrimSpace(StringValue(result["title"]))
		url := strings.TrimSpace(StringValue(result["url"]))
		content := strings.TrimSpace(StringValue(result["content"]))
		published := strings.TrimSpace(StringValue(result["published_date"]))
		state.Record(url, title)

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

		title := strings.TrimSpace(StringValue(result["title"]))
		url := strings.TrimSpace(StringValue(result["url"]))
		content := strings.TrimSpace(StringValue(result["content"]))
		published := strings.TrimSpace(StringValue(result["published_date"]))
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

		url := strings.TrimSpace(StringValue(page["url"]))
		content := strings.TrimSpace(StringValue(page["content"]))
		state.Record(url, "Fetched page")
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

		url := strings.TrimSpace(StringValue(page["url"]))
		content := strings.TrimSpace(StringValue(page["content"]))
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
		status := strings.TrimSpace(StringValue(result["status"]))
		if status == "completed" {
			continue
		}
		url := strings.TrimSpace(StringValue(result["url"]))
		errText := strings.TrimSpace(StringValue(result["error"]))
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
