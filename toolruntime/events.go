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

// webSearchCallEvent builds a single web_search_call output item for the
// non-streaming `output[]` array. The `blocked` status is collapsed to
// the spec-valid `failed` because OpenAI's web_search_call.status enum
// has no `blocked` slot; the unfiltered status still rides on the
// `_tinfoil` sidecar for clients that want to distinguish a safety
// block from a generic failure.
func webSearchCallEvent(id, status, errorCode string, action map[string]any) map[string]any {
	item := map[string]any{
		"type":   "web_search_call",
		"id":     id,
		"status": status,
		"action": action,
	}
	if status == "blocked" {
		item["status"] = "failed"
	}
	if sidecar := tinfoilSidecar(status, errorCode); sidecar != nil {
		item["_tinfoil"] = sidecar
	}
	return item
}

// tinfoilSidecar builds the `_tinfoil` vendor-extension field that rides
// alongside a web_search_call item on the Responses API. Strict OpenAI
// SDKs ignore unknown object fields, so this is invisible to clients
// that don't opt into reading it; clients that do (tinfoil-webapp,
// tinfoil-ios, anyone building a richer error UI on top of Tinfoil) can
// consume it directly off `item._tinfoil` with no additional opt-in.
//
// The sidecar carries ONLY information the spec cannot express:
//   - `status`: the unfiltered router status, which may be `blocked`
//     (distinct from the spec-valid `failed` that rides on the envelope
//     `status` field). Present only when the router status differs from
//     the envelope status, i.e., only on `blocked` today.
//   - `error.code`: an opaque router error code (e.g.
//     `blocked_by_safety_filter`) for clients that want to branch UI on
//     the specific reason. Present only when the tool call errored.
//
// Returns nil when there is nothing worth surfacing (the default, happy
// path) so the `_tinfoil` field is simply omitted from the item and the
// serialized JSON stays minimal for successful searches.
func tinfoilSidecar(rawStatus, errorCode string) map[string]any {
	if rawStatus != "blocked" && errorCode == "" {
		return nil
	}
	sidecar := map[string]any{}
	if rawStatus == "blocked" {
		sidecar["status"] = rawStatus
	}
	if errorCode != "" {
		sidecar["error"] = map[string]any{"code": errorCode}
	}
	return sidecar
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
