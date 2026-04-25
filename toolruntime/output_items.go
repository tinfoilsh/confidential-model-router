package toolruntime

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// toolCallLog accumulates the router-owned tool calls executed during a
// request so finalize can surface them as tinfoil-event markers (Chat)
// or spec-defined output items (Responses). It is intentionally separate
// from citationState, which tracks URL sources for annotation matching.
type toolCallLog struct {
	records []toolCallRecord
}

// record appends a completed tool call.
func (l *toolCallLog) record(r toolCallRecord) {
	if l == nil {
		return
	}
	l.records = append(l.records, r)
}

// toolCallRecord captures a tool call the router made on the user's behalf,
// used to surface web_search_call progress items to clients. errorReason
// carries the tool-side error message when the call failed so terminal
// web_search_call items can honestly report status:"failed" instead of
// silently claiming completion on a request that never returned results.
//
// resultSources carries the ordered {url, title} pairs this specific call
// produced (search results, fetched pages) so terminal tinfoil-event
// markers can attribute sources to the exact search call that surfaced
// them. resultURLs is the same list in URL-only form kept for the
// Responses API `action.sources` shape.
type toolCallRecord struct {
	name          string
	arguments     map[string]any
	resultURLs    []string
	resultSources []toolCallSource
	errorReason   string
	output        string // raw tool output text; used by code-exec events
}

// toolCallSource is a single {url, title} pair produced by a router tool
// call (a search hit or a fetched page). Titles are best-effort: a
// missing or empty title is surfaced as an empty string so clients can
// fall back to displaying the bare URL.
type toolCallSource struct {
	url   string
	title string
}

// statusForRecord maps a recorded router tool call to the web_search_call
// status surfaced to clients. "blocked" is reserved for PII or prompt
// injection safeguards so client UIs can surface a distinct affordance for
// the user; any other error becomes "failed".
func statusForRecord(record toolCallRecord) string {
	if record.errorReason == "" {
		return "completed"
	}
	if record.errorReason == blockedToolErrorReason {
		return "blocked"
	}
	return "failed"
}

// toActionSources wraps a list of URLs in the OpenAI-spec shape
// `[{type: "url", url: "..."}]` documented for `WebSearchCall.action.sources`.
// Returns nil when the input is empty so callers can omit the field entirely.
func toActionSources(urls []string) []any {
	if len(urls) == 0 {
		return nil
	}
	sources := make([]any, 0, len(urls))
	for _, url := range urls {
		if url == "" {
			continue
		}
		sources = append(sources, map[string]any{
			"type": "url",
			"url":  url,
		})
	}
	if len(sources) == 0 {
		return nil
	}
	return sources
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

// codeInterpreterCallEvent builds a single code_interpreter_call output
// item for the non-streaming Responses `output[]` array, analogous to
// webSearchCallEvent. The item follows OpenAI's code_interpreter_call
// shape: `code` carries a description of what was executed (tool name +
// serialized arguments), and `results` carries the text output.
func codeInterpreterCallEvent(id, status, toolName string, arguments map[string]any, output, errorCode string) map[string]any {
	code := toolName
	if len(arguments) > 0 {
		if argsJSON, err := json.Marshal(arguments); err == nil {
			code = toolName + ": " + string(argsJSON)
		}
	}
	item := map[string]any{
		"type":   "code_interpreter_call",
		"id":     id,
		"status": status,
		"code":   code,
	}
	if status == "blocked" {
		item["status"] = "failed"
	}
	if output != "" {
		item["results"] = []map[string]any{
			{"type": "text", "text": output},
		}
	}
	if sidecar := tinfoilSidecar(status, errorCode); sidecar != nil {
		item["_tinfoil"] = sidecar
	}
	return item
}

// buildWebSearchCallOutputItems turns recorded router tool calls into the
// web_search_call output items documented by OpenAI's Responses API.
//   - search tool calls become one action.type:"search" event with the query
//     and (when the caller opted in via `include:
//     ["web_search_call.action.sources"]`) the resolved source URLs in the
//     spec shape `[{type:"url", url:"..."}]`.
//   - fetch tool calls become one action.type:"open_page" event per URL so
//     clients can correlate each fetched page with its surrounding search.
func buildWebSearchCallOutputItems(records []toolCallRecord, includeActionSources bool) []any {
	events := make([]any, 0, len(records))
	for _, record := range records {
		status := statusForRecord(record)
		switch {
		case isRouterSearchToolName(record.name):
			action := map[string]any{"type": "search"}
			if query := stringValue(record.arguments["query"]); query != "" {
				action["query"] = query
			}
			if includeActionSources {
				if sources := toActionSources(record.resultURLs); len(sources) > 0 {
					action["sources"] = sources
				}
			}
			events = append(events, webSearchCallEvent("ws_"+uuid.NewString(), status, record.errorReason, action))
		case isRouterFetchToolName(record.name):
			for _, url := range fetchArgumentURLs(record.arguments) {
				action := map[string]any{"type": "open_page", "url": url}
				events = append(events, webSearchCallEvent("ws_"+uuid.NewString(), status, record.errorReason, action))
			}
		}
	}
	return events
}

// buildCodeInterpreterCallOutputItems turns recorded code-execution tool
// calls into code_interpreter_call output items for the non-streaming
// Responses API response. Records whose name is "search" or "fetch" are
// skipped because those belong to the web_search family.
func buildCodeInterpreterCallOutputItems(records []toolCallRecord) []any {
	var events []any
	for _, record := range records {
		if isWebSearchTool(record.name) {
			continue
		}
		status := statusForRecord(record)
		events = append(events, codeInterpreterCallEvent(
			"ci_"+uuid.NewString(), status, record.name, record.arguments, record.output, record.errorReason,
		))
	}
	return events
}

// list returns the recorded tool calls, or nil if the log is nil.
func (l *toolCallLog) list() []toolCallRecord {
	if l == nil {
		return nil
	}
	return l.records
}

// attachChatOutput walks each choice once, normalizes citation links,
// computes url_citation annotations, and prepends any enabled tinfoil-event
// marker families. Adding a new marker type is one if-block in the prefix
// builder; the choice iteration and annotation shift happen exactly once.
func attachChatOutput(body map[string]any, citations *citationState, toolCalls *toolCallLog, eventFlags tinfoilEventFlags) {
	if body == nil || citations == nil {
		return
	}
	records := toolCalls.list()
	choices, _ := body["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			continue
		}
		content := stringValue(message["content"])
		if normalized := normalizeCitationLinks(content); normalized != content {
			content = normalized
		}
		annotations := citations.nestedAnnotationsFor(content)

		// --- marker prefix: one block per tool type ---
		var prefix string
		if eventFlags.webSearch {
			prefix += tinfoilEventMarkersForRecords(records)
		}
		if eventFlags.codeExecution {
			prefix += tinfoilToolCallMarkersForRecords(records)
		}

		if prefix != "" {
			content = prefix + content
			shiftNestedAnnotationIndices(annotations, utf8.RuneCountInString(prefix))
		}
		message["content"] = content
		if len(annotations) > 0 {
			message["annotations"] = annotations
		}
	}
}

// attachResponsesOutput walks output items once, normalizes citation links,
// computes flat url_citation annotations, and prepends tool-call output
// items for every recorded router-owned call. Adding a new output-item type
// is one call to its builder; the annotation walk and the prepend happen
// exactly once.
func attachResponsesOutput(body map[string]any, citations *citationState, toolCalls *toolCallLog, includeActionSources bool) {
	if body == nil || citations == nil {
		return
	}
	outputItems, _ := body["output"].([]any)
	for _, rawItem := range outputItems {
		item, _ := rawItem.(map[string]any)
		if item == nil || stringValue(item["type"]) != "message" {
			continue
		}
		contentList, _ := item["content"].([]any)
		for _, rawContent := range contentList {
			contentMap, _ := rawContent.(map[string]any)
			if contentMap == nil || stringValue(contentMap["type"]) != "output_text" {
				continue
			}
			text := stringValue(contentMap["text"])
			if normalized := normalizeCitationLinks(text); normalized != text {
				contentMap["text"] = normalized
				text = normalized
			}
			annotations := citations.flatAnnotationsFor(text)
			if len(annotations) > 0 {
				contentMap["annotations"] = annotations
			}
		}
	}

	records := toolCalls.list()
	if len(records) == 0 {
		return
	}

	// --- output items: one builder per tool type ---
	wsEvents := buildWebSearchCallOutputItems(records, includeActionSources)
	ciEvents := buildCodeInterpreterCallOutputItems(records)

	if len(wsEvents) == 0 && len(ciEvents) == 0 {
		return
	}
	prepended := make([]any, 0, len(wsEvents)+len(ciEvents)+len(outputItems))
	prepended = append(prepended, wsEvents...)
	prepended = append(prepended, ciEvents...)
	prepended = append(prepended, outputItems...)
	body["output"] = prepended
}
