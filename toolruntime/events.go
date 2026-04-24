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

// tinfoilEventFlags tracks which opt-in marker families the caller
// requested via the `X-Tinfoil-Events` header. Each field gates the
// corresponding marker type so a client that only understands web
// search markers does not see unrelated markers.
type tinfoilEventFlags struct {
	webSearch bool
}

// parseTinfoilEventFlags parses the `X-Tinfoil-Events` header into per-family
// booleans. The header value is a comma-separated list of families the
// client is prepared to parse. We match case-insensitively and tolerate
// whitespace. A missing or empty header means the client gets a pristine
// spec-conformant stream with no markers at all.
func parseTinfoilEventFlags(h http.Header) tinfoilEventFlags {
	if h == nil {
		return tinfoilEventFlags{}
	}
	raw := h.Get(tinfoilEventsHeader)
	if raw == "" {
		return tinfoilEventFlags{}
	}
	var flags tinfoilEventFlags
	for _, part := range strings.Split(raw, ",") {
		family := strings.TrimSpace(part)
		if strings.EqualFold(family, tinfoilEventsWebSearch) {
			flags.webSearch = true
		}
	}
	return flags
}

// tinfoilEventsEnabled reports whether the caller opted into web-search
// tinfoil-event markers. Retained for call sites that only need the
// web-search gate.
func tinfoilEventsEnabled(h http.Header) bool {
	return parseTinfoilEventFlags(h).webSearch
}

// tinfoilEventMarker renders a single progress event as a text marker:
// a JSON payload wrapped in `<tinfoil-event>...</tinfoil-event>` tags
// padded with `\n` on both sides. The pad is part of the strip-regex
// contract used by the tinfoil-events parsers in the webapp and iOS
// apps and must not be changed without co-updating those parsers.
//
// When sources is non-empty the marker payload gains a top-level
// `sources` array of `{url, title}` objects so clients can attribute
// the citations produced by this specific tool call (e.g. one search
// among several in a multi-step turn) rather than having to merge all
// sources into one bucket per turn. Older clients ignore unknown keys.
func tinfoilEventMarker(id, status string, action map[string]any, reason string, sources []toolCallSource) string {
	payload := map[string]any{
		"type":    tinfoilEventPayloadType,
		"item_id": id,
		"status":  status,
		"action":  action,
	}
	if reason != "" {
		payload["error"] = map[string]any{"code": reason}
	}
	if encoded := encodeMarkerSources(sources); len(encoded) > 0 {
		payload["sources"] = encoded
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

// encodeMarkerSources maps toolCallSource into the plain-map shape used
// on the wire. Entries with an empty URL are dropped because a source
// without a target is not usable by a client. Titles are kept as-is,
// including empty strings, so clients can choose to fall back to the
// hostname rather than guess.
func encodeMarkerSources(sources []toolCallSource) []map[string]any {
	if len(sources) == 0 {
		return nil
	}
	encoded := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		if source.url == "" {
			continue
		}
		encoded = append(encoded, map[string]any{
			"url":   source.url,
			"title": source.title,
		})
	}
	if len(encoded) == 0 {
		return nil
	}
	return encoded
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
	writePair := func(action map[string]any, s, reason string, sources []toolCallSource) {
		id := "ws_" + uuid.NewString()
		builder.WriteString(tinfoilEventMarker(id, "in_progress", action, "", nil))
		builder.WriteString(tinfoilEventMarker(id, s, action, reason, sources))
	}
	for _, record := range records {
		switch {
		case isRouterSearchToolName(record.name):
			action := map[string]any{"type": "search"}
			if query := stringValue(record.arguments["query"]); query != "" {
				action["query"] = query
			}
			writePair(action, status(record), record.errorReason, record.resultSources)
		case isRouterFetchToolName(record.name):
			urls := fetchArgumentURLs(record.arguments)
			if len(urls) == 0 {
				continue
			}
			for _, url := range urls {
				action := map[string]any{"type": "open_page", "url": url}
				writePair(action, status(record), record.errorReason, nil)
			}
		}
	}
	return builder.String()
}


