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
//
// Why collapse `blocked` onto `failed` here?
//   - OpenAI's documented `web_search_call.status` enum is {in_progress,
//     searching, completed, failed}. There is no `blocked` slot, so any
//     client validating responses against the published schema (SDKs
//     that expose the enum as a sealed type, JSON-schema gateways that
//     reject unknown values, etc.) would reject a response carrying
//     `status: "blocked"`.
//   - We still want opt-in clients to distinguish a safety-filter block
//     from a generic error so UIs can render a different affordance, so
//     the unfiltered `blocked` string lives inside the tinfoil-event
//     marker payload carried in-band. Clients that never opt into the
//     marker stream never learn about `blocked` and see only the
//     spec-valid `failed` status, which is semantically truthful: the
//     call did not complete successfully.
//   - Keeping the collapse at the leaf (this helper) rather than at the
//     caller guarantees no future caller can accidentally ship a
//     schema-invalid `blocked` status on an output item no matter which
//     status-producing path it used.
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
//
// Why pad the marker with `\n` on both sides?
//   - OpenAI SDKs that do not recognize the tinfoil-event tags render
//     them verbatim as part of the assistant text. A marker that
//     abutted surrounding prose would visually merge into a sentence,
//     creating a jarring in-line artifact on unopted-in clients and
//     breaking the `text before marker` / `text after marker` visual
//     separation that makes the stream legible during human debugging.
//   - Opt-in clients strip the marker using a regex that matches
//     `\n?<tinfoil-event>...</tinfoil-event>\n?`. The pad-newlines get
//     absorbed by the strip so there is no visible gap or double blank
//     line in the rendered output; the text before and after the
//     marker collapses back together seamlessly.
//   - Carrying the markers on their own line also means a streaming
//     client can parse SSE frame-by-frame and detect a complete marker
//     without needing to buffer across arbitrary partial frames: the
//     close tag and its trailing newline arrive in the same delta as
//     the open tag in practice, and at worst span two deltas that the
//     parser already buffers.
//
// The parser tuned to these semantics lives in the tinfoil-events
// packages the webapp and iOS apps import; the `\n` pad is part of that
// contract and should not be trimmed without co-updating those parsers.
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
