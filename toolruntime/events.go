package toolruntime

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
)


// Tinfoil-event marker constants. The router emits opt-in progress
// markers for router-owned tool calls (web search, fetch) by wrapping a
// JSON payload in `<tinfoil-event>...</tinfoil-event>` tags carried
// inside the assistant text stream. Every carrier is a spec-conformant
// frame (chat delta content, response.output_text.delta, or final
// assistant message text) so OpenAI SDKs that do not recognize the tags
// simply render them as text. Clients that do recognize the tags strip
// them with a single regex before rendering. The marker is only emitted
// when the caller opts in via the tinfoilEventsHeader request header.
const (
	tinfoilEventsHeader     = "X-Tinfoil-Events"
	tinfoilEventsWebSearch  = "web_search"
	tinfoilEventOpenTag     = "<tinfoil-event>"
	tinfoilEventCloseTag    = "</tinfoil-event>"
	tinfoilEventPayloadType = "tinfoil.web_search_call"
)

// webSearchCallEvent builds a single web_search_call output item in the
// spec-conformant shape documented by OpenAI for the Responses API. The
// returned map is used only for non-streaming `output[]` items; it does
// NOT carry the `item_id` / `reason` fields the streaming envelopes use,
// and it maps the router's internal `blocked` status onto the spec-valid
// `failed` status (the safety-block signal is surfaced separately via
// tinfoil-event markers when the caller opts in).
func webSearchCallEvent(id, status string, action map[string]any) map[string]any {
	if status == "blocked" {
		status = "failed"
	}
	return map[string]any{
		"type":   "web_search_call",
		"id":     id,
		"status": status,
		"action": action,
	}
}

// tinfoilEventsEnabled reports whether the caller opted into the
// router-owned tinfoil-event marker stream for web-search tool calls.
// The header value is a comma-separated list of families the client is
// prepared to parse. We match case-insensitively and tolerate whitespace.
// A missing or empty header means the client gets a pristine
// spec-conformant stream with no markers at all.
func tinfoilEventsEnabled(h http.Header) bool {
	if h == nil {
		return false
	}
	raw := h.Get(tinfoilEventsHeader)
	if raw == "" {
		return false
	}
	for _, part := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(part), tinfoilEventsWebSearch) {
			return true
		}
	}
	return false
}

// tinfoilEventMarker renders a single progress event as a text marker:
// a JSON payload wrapped in `<tinfoil-event>...</tinfoil-event>` tags on
// its own line. The leading and trailing newlines isolate the marker
// from natural-language text so client strip regexes do not leave stray
// whitespace, and so raw SSE captures stay readable for debugging.
func tinfoilEventMarker(id, status string, action map[string]any, reason string) string {
	payload := map[string]any{
		"type":    tinfoilEventPayloadType,
		"item_id": id,
		"status":  status,
		"action":  action,
	}
	if reason != "" {
		payload["error"] = map[string]any{"code": reason}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal on a map with string keys and primitive /
		// map[string]any values cannot fail in practice. Fall back to
		// an empty payload rather than panicking so a bug here can
		// never break the main stream for a client.
		data = []byte(`{"type":"` + tinfoilEventPayloadType + `"}`)
	}
	return "\n" + tinfoilEventOpenTag + string(data) + tinfoilEventCloseTag + "\n"
}

// tinfoilEventMarkersForRecords renders a sequence of in_progress then
// terminal markers (completed / failed / blocked) for the non-streaming
// paths, which only see recorded tool calls after they have already run.
// Each recorded `search` call yields one marker pair; each recorded
// `fetch` call yields one marker pair per URL so clients see the same
// progression as the streaming paths. The non-streaming carrier keeps
// the distinct `blocked` status inside the marker JSON even though the
// spec-conformant `web_search_call` output item collapses it onto
// `failed` — the whole point of the marker is to surface details the
// spec has no slot for.
func tinfoilEventMarkersForRecords(records []toolCallRecord) string {
	if len(records) == 0 {
		return ""
	}
	var builder strings.Builder
	status := func(record toolCallRecord) string { return statusForRecord(record) }
	writePair := func(action map[string]any, s, reason string) {
		id := "ws_" + uuid.NewString()
		builder.WriteString(tinfoilEventMarker(id, "in_progress", action, ""))
		builder.WriteString(tinfoilEventMarker(id, s, action, reason))
	}
	for _, record := range records {
		switch record.name {
		case "search":
			action := map[string]any{"type": "search"}
			if query := stringValue(record.arguments["query"]); query != "" {
				action["query"] = query
			}
			writePair(action, status(record), record.errorReason)
		case "fetch":
			urls := fetchArgumentURLs(record.arguments)
			if len(urls) == 0 {
				continue
			}
			for _, url := range urls {
				action := map[string]any{"type": "open_page", "url": url}
				writePair(action, status(record), record.errorReason)
			}
		}
	}
	return builder.String()
}

// fetchArgumentURLs extracts the string URLs the model handed the fetch tool.
func fetchArgumentURLs(arguments map[string]any) []string {
	raw, ok := arguments["urls"].([]any)
	if !ok {
		return nil
	}
	urls := make([]string, 0, len(raw))
	for _, item := range raw {
		if url := strings.TrimSpace(stringValue(item)); url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}
