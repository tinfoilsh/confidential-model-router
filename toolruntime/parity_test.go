package toolruntime

import (
	"regexp"
	"strings"
	"testing"
)

// TestChatAndResponsesParityOnSameToolCalls pins that a canonical set of
// recorded router tool calls surfaces aligned information on both API
// surfaces despite differing carriers. Chat uses the opt-in tinfoil-event
// marker stream for live progress (since chat has no spec slot for
// web_search_call); Responses uses the spec-defined web_search_call
// output items. The assertion here is that the terminal status set on
// each surface matches one-for-one (with chat's `blocked` collapsed onto
// the spec-valid `failed` for comparison), and that citation URLs and
// visible text are identical across surfaces.
func TestChatAndResponsesParityOnSameToolCalls(t *testing.T) {
	t.Parallel()

	const assistantText = "The sky is blue [Example](https://example.com/article)."

	buildCitationState := func() *citationState {
		c := &citationState{nextIndex: 1}
		c.record("https://example.com/article", "Example")
		c.recordToolCall(toolCallRecord{
			name:      "search",
			arguments: map[string]any{"query": "why is the sky blue"},
			resultURLs: []string{
				"https://example.com/article",
				"https://example.com/other",
			},
		})
		c.recordToolCall(toolCallRecord{
			name:      "fetch",
			arguments: map[string]any{"urls": []any{"https://example.com/article"}},
		})
		c.recordToolCall(toolCallRecord{
			name:        "search",
			arguments:   map[string]any{"query": "blocked query"},
			errorReason: blockedToolErrorReason,
		})
		return c
	}

	// Chat surface: tinfoil-event markers are injected into the
	// assistant content when the caller opted in via the header.
	chatCitations := buildCitationState()
	chatBody := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": assistantText},
			},
		},
	}
	attachChatCitations(chatBody, chatCitations, true)

	// Responses surface: the spec-conformant web_search_call output
	// items carry progress, no tinfoil markers ride on this path.
	respCitations := buildCitationState()
	respBody := map[string]any{
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": assistantText},
				},
			},
		},
	}
	attachResponsesCitations(respBody, respCitations, true)

	// Citation URL set must match exactly across surfaces.
	chatURLs := extractChatCitationURLs(t, chatBody)
	respURLs := extractResponsesCitationURLs(t, respBody)
	if !stringSetEqual(chatURLs, respURLs) {
		t.Fatalf("citation URLs differ: chat=%v responses=%v", chatURLs, respURLs)
	}
	if len(chatURLs) == 0 {
		t.Fatal("expected at least one url_citation annotation on both surfaces")
	}

	// Visible assistant text on both surfaces must be identical once
	// chat's tinfoil-event markers are stripped (Responses never
	// carries markers at all so its visible text equals the raw text).
	chatContent := firstChatContent(t, chatBody)
	respContent := firstResponsesOutputText(t, respBody)
	chatVisible := stripMarkers(chatContent)
	respVisible := strings.TrimSpace(respContent)
	if chatVisible != respVisible {
		t.Fatalf("visible text differs across surfaces:\nchat:      %q\nresponses: %q", chatVisible, respVisible)
	}

	// Terminal status alignment: the chat marker sequence's terminal
	// statuses must match the Responses spec web_search_call statuses
	// one-for-one, with chat's router-specific `blocked` status
	// collapsed onto the spec-valid `failed` (OpenAI's
	// web_search_call.status enum has no dedicated blocked slot).
	chatTerminals := collapseBlockedTerminalStatuses(extractMarkerStatuses(t, chatContent))
	specStatuses := extractSpecStatuses(t, respBody)
	if !stringSliceEqual(chatTerminals, specStatuses) {
		t.Fatalf("terminal status set differs:\n chat terminals (blocked->failed): %v\n responses spec statuses:          %v", chatTerminals, specStatuses)
	}
}

// extractChatCitationURLs pulls every url_citation URL off the nested Chat
// Completions shape: choices[].message.annotations[].url_citation.url.
func extractChatCitationURLs(t *testing.T, body map[string]any) []string {
	t.Helper()
	var urls []string
	choices, _ := body["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		anns, _ := message["annotations"].([]any)
		for _, rawAnn := range anns {
			ann, _ := rawAnn.(map[string]any)
			inner, _ := ann["url_citation"].(map[string]any)
			if u := stringValue(inner["url"]); u != "" {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// extractResponsesCitationURLs pulls every url_citation URL off the flat
// Responses API shape: output[].content[].annotations[].url.
func extractResponsesCitationURLs(t *testing.T, body map[string]any) []string {
	t.Helper()
	var urls []string
	items, _ := body["output"].([]any)
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if stringValue(item["type"]) != "message" {
			continue
		}
		contents, _ := item["content"].([]any)
		for _, rawContent := range contents {
			content, _ := rawContent.(map[string]any)
			anns, _ := content["annotations"].([]any)
			for _, rawAnn := range anns {
				ann, _ := rawAnn.(map[string]any)
				if u := stringValue(ann["url"]); u != "" {
					urls = append(urls, u)
				}
			}
		}
	}
	return urls
}

// extractSpecStatuses pulls the status string off every web_search_call
// output item on the Responses body, in encounter order.
func extractSpecStatuses(t *testing.T, body map[string]any) []string {
	t.Helper()
	var statuses []string
	items, _ := body["output"].([]any)
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if stringValue(item["type"]) != "web_search_call" {
			continue
		}
		statuses = append(statuses, stringValue(item["status"]))
	}
	return statuses
}

// firstChatContent returns the content string on the first choice's
// assistant message, which is where attachChatCitations injects markers.
func firstChatContent(t *testing.T, body map[string]any) string {
	t.Helper()
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("chat body has no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return stringValue(message["content"])
}

// firstResponsesOutputText returns the first output_text `text` field in
// the Responses body, which is where attachResponsesCitations prefixes
// the marker sequence onto the assistant message.
func firstResponsesOutputText(t *testing.T, body map[string]any) string {
	t.Helper()
	items, _ := body["output"].([]any)
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if stringValue(item["type"]) != "message" {
			continue
		}
		contents, _ := item["content"].([]any)
		for _, rawContent := range contents {
			content, _ := rawContent.(map[string]any)
			if stringValue(content["type"]) == "output_text" {
				return stringValue(content["text"])
			}
		}
	}
	t.Fatal("responses body has no output_text content")
	return ""
}

// tinfoilEventMarkerPattern matches the on-the-wire marker shape
// (newline, open tag, JSON, close tag, newline) so tests can extract
// payloads without committing to a specific JSON field order.
var tinfoilEventMarkerPattern = regexp.MustCompile(`<tinfoil-event>(\{[^<]*?\})</tinfoil-event>`)

// extractMarkerStatuses returns the `status` value of every tinfoil-event
// marker found in text, in encounter order.
func extractMarkerStatuses(t *testing.T, text string) []string {
	t.Helper()
	matches := tinfoilEventMarkerPattern.FindAllStringSubmatch(text, -1)
	statuses := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		// Cheap status extraction without a full JSON decode so this
		// helper does not depend on encoding/json field ordering in
		// the marker payload.
		payload := m[1]
		idx := strings.Index(payload, `"status":"`)
		if idx < 0 {
			continue
		}
		rest := payload[idx+len(`"status":"`):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			continue
		}
		statuses = append(statuses, rest[:end])
	}
	return statuses
}

// stripMarkers removes every `<tinfoil-event>...</tinfoil-event>` block
// and the surrounding marker newlines from text so tests can compare the
// user-visible remainder across surfaces.
func stripMarkers(text string) string {
	stripped := tinfoilEventMarkerPattern.ReplaceAllString(text, "")
	stripped = strings.ReplaceAll(stripped, "\n\n", "")
	return strings.TrimSpace(stripped)
}

// collapseBlockedTerminalStatuses maps the in_progress+terminal marker
// sequence onto the spec-compliant web_search_call status list. Only the
// terminal statuses survive (one per recorded tool call) and any router
// `blocked` status collapses onto the spec's `failed` status because
// OpenAI's web_search_call schema has no dedicated slot for it.
func collapseBlockedTerminalStatuses(markerStatuses []string) []string {
	out := make([]string, 0, len(markerStatuses)/2)
	for _, s := range markerStatuses {
		if s == "in_progress" {
			continue
		}
		if s == "blocked" {
			s = "failed"
		}
		out = append(out, s)
	}
	return out
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
