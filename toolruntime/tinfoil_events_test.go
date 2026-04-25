package toolruntime

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestTinfoilEventsEnabledHeaderParsing pins the header gate: the marker
// stream is only active when `X-Tinfoil-Events` lists `web_search`
// (case-insensitive) as one of its comma-separated families. An empty
// or unrelated header must leave the streams pristine.
func TestTinfoilEventsEnabledHeaderParsing(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		enable bool
	}{
		{"empty", "", false},
		{"unrelated", "audio_progress", false},
		{"exact", "web_search", true},
		{"exact upper", "WEB_SEARCH", true},
		{"comma list with web_search", "audio_progress, web_search ,misc", true},
		{"comma list without web_search", "audio_progress,misc", false},
		{"whitespace padded", "  web_search  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set(tinfoilEventsHeader, tc.value)
			}
			if got := tinfoilEventsEnabled(h); got != tc.enable {
				t.Fatalf("tinfoilEventsEnabled(%q) = %v, want %v", tc.value, got, tc.enable)
			}
		})
	}
}

// TestTinfoilEventMarkerShape pins the on-the-wire format: every marker
// is a standalone line wrapped in `<tinfoil-event>...</tinfoil-event>`
// with a minified JSON payload carrying the web_search_call shape.
// Parsing the payload as JSON must succeed and round-trip the fields
// callers depend on (id, status, action) plus the tinfoil-specific
// reason when present.
func TestTinfoilEventMarkerShape(t *testing.T) {
	marker := tinfoilEventMarker("ws_1", "in_progress", map[string]any{"type": "search", "query": "q"}, "", nil)
	if !strings.HasPrefix(marker, "\n"+tinfoilEventOpenTag) {
		t.Fatalf("marker must start on its own line with %q: got %q", tinfoilEventOpenTag, marker)
	}
	if !strings.HasSuffix(marker, tinfoilEventCloseTag+"\n") {
		t.Fatalf("marker must end on its own line with %q: got %q", tinfoilEventCloseTag, marker)
	}

	payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\n"+tinfoilEventOpenTag), tinfoilEventCloseTag+"\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v (payload=%q)", err, payload)
	}
	if decoded["type"] != tinfoilEventPayloadType {
		t.Fatalf("payload.type = %v, want %q", decoded["type"], tinfoilEventPayloadType)
	}
	if decoded["item_id"] != "ws_1" {
		t.Fatalf("payload.item_id = %v, want ws_1", decoded["item_id"])
	}
	if decoded["status"] != "in_progress" {
		t.Fatalf("payload.status = %v, want in_progress", decoded["status"])
	}
	action, _ := decoded["action"].(map[string]any)
	if action == nil || action["type"] != "search" || action["query"] != "q" {
		t.Fatalf("payload.action malformed: %#v", decoded["action"])
	}
	if _, present := decoded["error"]; present {
		t.Fatalf("payload must not include error when reason was empty")
	}
}

// TestTinfoilEventMarkerIncludesReason pins that the optional reason
// field rides on the marker only when non-empty, so opt-in clients can
// surface safety-block text or upstream error context without the field
// polluting every success marker.
func TestTinfoilEventMarkerIncludesReason(t *testing.T) {
	marker := tinfoilEventMarker("ws_1", "blocked", map[string]any{"type": "search"}, "safety policy rejected this query", nil)
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\n"+tinfoilEventOpenTag), tinfoilEventCloseTag+"\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v", err)
	}
	errMap, _ := decoded["error"].(map[string]any)
	if errMap == nil {
		t.Fatalf("payload.error must be present for blocked status, got %#v", decoded)
	}
	if errMap["code"] != "safety policy rejected this query" {
		t.Fatalf("payload.error.code = %v, want blocked detail", errMap["code"])
	}
}

// TestTinfoilEventMarkersForRecordsMapsSearch pins the non-streaming
// bulk-marker assembly for a search: one in_progress + one terminal
// marker are produced per recorded search, in the order the tool was
// invoked. Multiple records concatenate without losing the per-marker
// newline separator, so a regex strip recovers the original content
// cleanly.
func TestTinfoilEventMarkersForRecordsMapsSearch(t *testing.T) {
	records := []toolCallRecord{
		{name: "search", arguments: map[string]any{"query": "x"}},
	}
	combined := tinfoilEventMarkersForRecords(records)
	occurrences := strings.Count(combined, tinfoilEventOpenTag)
	if occurrences != 2 {
		t.Fatalf("expected 2 markers (in_progress + completed), got %d in %q", occurrences, combined)
	}
	if !strings.Contains(combined, `"status":"in_progress"`) {
		t.Fatalf("missing in_progress marker: %q", combined)
	}
	if !strings.Contains(combined, `"status":"completed"`) {
		t.Fatalf("missing completed marker: %q", combined)
	}
}

// TestExtractToolOutputSourcesPairsSourceAndURL pins that the
// formatted search tool output is parsed into the correct ordered
// {url, title} list so terminal markers can attribute citations to
// the call that produced them. URLs missing a `Source:` line yield an
// empty title; duplicate URLs are dropped.
func TestExtractToolOutputSourcesPairsSourceAndURL(t *testing.T) {
	output := strings.Join([]string{
		"Source: First Hit",
		"URL: https://one.example",
		"snippet one",
		"",
		"URL: https://no-title.example",
		"snippet two",
		"",
		"Source: Third Hit",
		"URL: https://one.example",
		"duplicate url should be ignored",
	}, "\n")

	sources := extractToolOutputSources(output)
	if len(sources) != 2 {
		t.Fatalf("expected 2 unique sources, got %d: %#v", len(sources), sources)
	}
	if sources[0].url != "https://one.example" || sources[0].title != "First Hit" {
		t.Fatalf("first source mismatch: %#v", sources[0])
	}
	if sources[1].url != "https://no-title.example" || sources[1].title != "" {
		t.Fatalf("second source mismatch: %#v", sources[1])
	}
}

// TestTinfoilEventMarkerEmbedsSources pins that the optional sources
// list surfaces on the marker JSON as a `sources` array of
// `{url, title}` objects only on the terminal marker. Opt-in clients
// use this to attribute citations to the specific search call that
// produced them when a single turn runs multiple searches.
func TestTinfoilEventMarkerEmbedsSources(t *testing.T) {
	sources := []toolCallSource{
		{url: "https://one.example", title: "One"},
		{url: "https://two.example", title: "Two"},
	}
	marker := tinfoilEventMarker("ws_1", "completed", map[string]any{"type": "search", "query": "q"}, "", sources)
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\n"+tinfoilEventOpenTag), tinfoilEventCloseTag+"\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v (payload=%q)", err, payload)
	}
	rawSources, ok := decoded["sources"].([]any)
	if !ok {
		t.Fatalf("payload.sources missing or wrong type: %#v", decoded["sources"])
	}
	if got := len(rawSources); got != 2 {
		t.Fatalf("expected 2 sources, got %d: %#v", got, rawSources)
	}
	first, _ := rawSources[0].(map[string]any)
	if first["url"] != "https://one.example" || first["title"] != "One" {
		t.Fatalf("first source did not round-trip: %#v", first)
	}
}

// TestTinfoilEventMarkerOmitsSourcesWhenEmpty pins that the `sources`
// field is absent from the payload when no sources are supplied so the
// happy-path marker bytes stay minimal and existing snapshots continue
// to round-trip.
func TestTinfoilEventMarkerOmitsSourcesWhenEmpty(t *testing.T) {
	marker := tinfoilEventMarker("ws_1", "completed", map[string]any{"type": "search"}, "", nil)
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\n"+tinfoilEventOpenTag), tinfoilEventCloseTag+"\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v (payload=%q)", err, payload)
	}
	if _, present := decoded["sources"]; present {
		t.Fatalf("payload.sources must be omitted when no sources supplied, got %#v", decoded)
	}
}

// TestTinfoilEventMarkersForRecordsAttachesSources pins that the
// non-streaming bulk-marker emitter puts record.resultSources onto the
// terminal marker of a search record (and not onto the in_progress
// marker), so the non-streaming chat path matches the streaming one.
func TestTinfoilEventMarkersForRecordsAttachesSources(t *testing.T) {
	records := []toolCallRecord{
		{
			name:      "search",
			arguments: map[string]any{"query": "q"},
			resultSources: []toolCallSource{
				{url: "https://one.example", title: "One"},
			},
		},
	}
	combined := tinfoilEventMarkersForRecords(records)
	// Split into individual marker payloads and confirm only the
	// completed marker carries sources.
	matches := tinfoilEventMarkerPattern.FindAllStringSubmatch(combined, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 markers (in_progress + completed), got %d", len(matches))
	}
	for _, match := range matches {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(match[1]), &decoded); err != nil {
			t.Fatalf("marker payload must be valid JSON: %v (%q)", err, match[1])
		}
		status, _ := decoded["status"].(string)
		_, hasSources := decoded["sources"]
		if status == "in_progress" && hasSources {
			t.Fatalf("in_progress marker must not carry sources: %q", match[1])
		}
		if status == "completed" && !hasSources {
			t.Fatalf("completed marker must carry sources: %q", match[1])
		}
	}
}

// TestTinfoilEventMarkersForRecordsMapsFetch pins that a recorded fetch
// with three URLs emits six markers (in_progress + terminal per URL),
// matching the per-URL fan-out surfaced by the streaming emitter.
func TestTinfoilEventMarkersForRecordsMapsFetch(t *testing.T) {
	records := []toolCallRecord{
		{
			name: "fetch",
			arguments: map[string]any{"urls": []any{
				"https://a.example",
				"https://b.example",
				"https://c.example",
			}},
		},
	}
	combined := tinfoilEventMarkersForRecords(records)
	if got := strings.Count(combined, tinfoilEventOpenTag); got != 6 {
		t.Fatalf("expected 6 fetch markers (3 urls * 2 phases), got %d in %q", got, combined)
	}
	for _, url := range []string{"https://a.example", "https://b.example", "https://c.example"} {
		if !strings.Contains(combined, url) {
			t.Fatalf("fetch marker stream missing url %q: %q", url, combined)
		}
	}
}

// TestTinfoilEventMarkersForRecordsMapsBlocked pins that a recorded
// search whose errorReason is the safety-block constant surfaces as
// status:"blocked" on the marker (preserving the full status vocabulary
// for opt-in clients), even though the companion spec-conformant
// web_search_call output item collapses blocked onto failed.
func TestTinfoilEventMarkersForRecordsMapsBlocked(t *testing.T) {
	records := []toolCallRecord{
		{
			name:        "search",
			arguments:   map[string]any{"query": "sensitive"},
			errorReason: blockedToolErrorReason,
		},
	}
	combined := tinfoilEventMarkersForRecords(records)
	if !strings.Contains(combined, `"status":"blocked"`) {
		t.Fatalf("expected blocked marker for safety-block record: %q", combined)
	}
	if !strings.Contains(combined, blockedToolErrorReason) {
		t.Fatalf("expected marker reason to carry safety-block constant: %q", combined)
	}
}

// TestAttachChatCitationsInjectsMarkersWhenEnabled pins the non-streaming
// chat contract: when the caller opted into tinfoil-events, every recorded
// tool call's markers are prepended to the assistant message content and
// the companion url_citation annotation indices are rune-shifted so spans
// keep pointing at the right substrings of the combined content.
func TestAttachChatCitationsInjectsMarkersWhenEnabled(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/page", "Example page")
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

	original := "A claim [Example page](https://example.com/page) stands."
	body := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": original},
			},
		},
	}

	attachChatOutput(body, state, tc, tinfoilEventFlags{webSearch: true})

	message := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	content := message["content"].(string)
	if !strings.HasPrefix(content, "\n"+tinfoilEventOpenTag) {
		t.Fatalf("expected content to start with a marker when events enabled: %q", content)
	}
	if !strings.HasSuffix(content, original) {
		t.Fatalf("expected original content to be preserved after markers: %q", content)
	}

	annotations := message["annotations"].([]any)
	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(annotations))
	}
	citation := annotations[0].(map[string]any)["url_citation"].(map[string]any)
	// The label "Example page" starts at rune offset 8 inside the
	// original text; after prefixing the markers it must move forward
	// by exactly the rune count of the prefix.
	prefix := strings.TrimSuffix(content, original)
	// "A claim [" is 9 runes; the label "Example page" therefore starts
	// at rune index 9 in the pristine text. After prepending markers
	// the label must move forward by exactly the rune count of the
	// prefix.
	wantStart := 9 + countRunes(prefix)
	if got := citation["start_index"].(int); got != wantStart {
		t.Fatalf("annotation start_index = %d, want %d", got, wantStart)
	}
}

// TestAttachChatCitationsSkipsMarkersWhenDisabled pins the opt-out path:
// callers that do not set the header never see markers, even when the
// router ran router-owned tools that would have produced progress.
func TestAttachChatCitationsSkipsMarkersWhenDisabled(t *testing.T) {
	state := &citationState{nextIndex: 1}
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})
	body := map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"role": "assistant", "content": "Answer"}},
		},
	}

	attachChatOutput(body, state, tc, tinfoilEventFlags{})

	content := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if strings.Contains(content, tinfoilEventOpenTag) {
		t.Fatalf("markers must be absent when events are not enabled: %q", content)
	}
	if content != "Answer" {
		t.Fatalf("content must be pristine when events are not enabled: %q", content)
	}
}

// TestAttachResponsesCitationsNeverInjectsMarkers pins that the
// Responses path is always fully spec-conformant: no tinfoil-event
// markers ride on the assistant output_text even for recorded
// router-owned tool calls, and the spec web_search_call output items
// carry all progress information for the client.
func TestAttachResponsesCitationsNeverInjectsMarkers(t *testing.T) {
	state := &citationState{nextIndex: 1}
	state.record("https://example.com/page", "Example page")
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

	original := "A claim [Example page](https://example.com/page) stands."
	body := map[string]any{
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": original},
				},
			},
		},
	}

	attachResponsesOutput(body, state, tc, false)

	items := body["output"].([]any)
	// A web_search_call item is prepended, then the assistant message
	// is left pristine (no tinfoil-event markers on the Responses path).
	var messageItem map[string]any
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] == "message" {
			messageItem = item
			break
		}
	}
	if messageItem == nil {
		t.Fatalf("assistant message not found in %#v", items)
	}
	contentList := messageItem["content"].([]any)
	first := contentList[0].(map[string]any)
	text := first["text"].(string)
	if strings.Contains(text, tinfoilEventOpenTag) {
		t.Fatalf("Responses output_text must never contain tinfoil markers: %q", text)
	}
	if text == "" {
		t.Fatalf("Responses output_text must preserve the original assistant text: %q", text)
	}
	annotations := first["annotations"].([]any)
	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation on pristine text, got %d", len(annotations))
	}
	ann := annotations[0].(map[string]any)
	// "Example page" starts at rune index 9 in the original text; with
	// no prefix injected the annotation should point exactly there.
	if got := ann["start_index"].(int); got != 9 {
		t.Fatalf("annotation start_index = %d, want 9 (pristine text)", got)
	}
}

// TestAttachResponsesCitationsPrependsWebSearchCallItem pins the spec
// carrier for router-owned tool-call progress on the Responses path: a
// web_search_call output item is prepended for every recorded call, no
// synthetic tinfoil-event marker message is produced, and a blocked
// tool call surfaces the unfiltered router status on the `_tinfoil`
// vendor-extension sidecar so Tinfoil-aware clients can distinguish a
// safety-filter block from a generic failure.
func TestAttachResponsesCitationsPrependsWebSearchCallItem(t *testing.T) {
	state := &citationState{nextIndex: 1}
	tc := &toolCallLog{}
	tc.record(toolCallRecord{
		name:        "search",
		arguments:   map[string]any{"query": "sensitive"},
		errorReason: blockedToolErrorReason,
	})

	body := map[string]any{
		"output": []any{},
	}

	attachResponsesOutput(body, state, tc, false)

	items := body["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 output item (the web_search_call), got %d: %#v", len(items), items)
	}
	first := items[0].(map[string]any)
	if first["type"] != "web_search_call" {
		t.Fatalf("expected first item to be web_search_call, got %v", first["type"])
	}
	// OpenAI's spec has no `blocked` slot on the web_search_call.status
	// enum, so the envelope collapses onto the spec-valid `failed`.
	if first["status"] != "failed" {
		t.Fatalf("expected blocked tool call to collapse onto status=failed, got %v", first["status"])
	}
	// _tinfoil sidecar preserves the unfiltered router signal for
	// Tinfoil-aware clients. Strict OpenAI SDKs ignore this field.
	sidecar, ok := first["_tinfoil"].(map[string]any)
	if !ok {
		t.Fatalf("blocked item must carry _tinfoil sidecar, got %#v", first["_tinfoil"])
	}
	if sidecar["status"] != "blocked" {
		t.Fatalf("_tinfoil.status = %v, want blocked", sidecar["status"])
	}
	errObj, _ := sidecar["error"].(map[string]any)
	if errObj == nil || errObj["code"] != blockedToolErrorReason {
		t.Fatalf("_tinfoil.error.code = %v, want %q", errObj, blockedToolErrorReason)
	}
	// No synthetic tinfoil-event marker message should be injected.
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] == "message" {
			t.Fatalf("Responses path must not synthesize a tinfoil-event marker message: %#v", item)
		}
	}
}

// TestAttachResponsesCitationsSuccessfulCallOmitsTinfoilSidecar pins
// that the non-streaming final snapshot keeps successful web_search_call
// items minimal: no `_tinfoil` field when there is nothing worth
// surfacing beyond the spec envelope.
func TestAttachResponsesCitationsSuccessfulCallOmitsTinfoilSidecar(t *testing.T) {
	state := &citationState{nextIndex: 1}
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

	body := map[string]any{"output": []any{}}
	attachResponsesOutput(body, state, tc, false)

	items := body["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 web_search_call item, got %d: %#v", len(items), items)
	}
	first := items[0].(map[string]any)
	if first["status"] != "completed" {
		t.Fatalf("expected status=completed, got %v", first["status"])
	}
	if _, present := first["_tinfoil"]; present {
		t.Fatalf("successful call must omit _tinfoil sidecar: %#v", first)
	}
}

// TestAttachResponsesCitationsLeavesPristineText pins that the assistant
// output_text is never mutated with tinfoil-event markers on the
// Responses path, regardless of recorded tool calls.
func TestAttachResponsesCitationsLeavesPristineText(t *testing.T) {
	state := &citationState{nextIndex: 1}
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

	body := map[string]any{
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": "Answer"},
				},
			},
		},
	}

	attachResponsesOutput(body, state, tc, false)

	items := body["output"].([]any)
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] != "message" {
			continue
		}
		text := item["content"].([]any)[0].(map[string]any)["text"].(string)
		if strings.Contains(text, tinfoilEventOpenTag) {
			t.Fatalf("Responses output_text must never contain tinfoil markers: %q", text)
		}
		if text != "Answer" {
			t.Fatalf("Responses output_text must be pristine: %q", text)
		}
	}
}

// --------------- Code Execution Event Tests ---------------

// TestParseTinfoilEventFlagsCodeExecution pins that the
// `X-Tinfoil-Events` header accepts `code_execution` as a family.
func TestParseTinfoilEventFlagsCodeExecution(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		wantWS    bool
		wantCE    bool
	}{
		{"empty", "", false, false},
		{"web_search only", "web_search", true, false},
		{"code_execution only", "code_execution", false, true},
		{"both", "web_search,code_execution", true, true},
		{"both reversed", "code_execution, web_search", true, true},
		{"case insensitive", "CODE_EXECUTION", false, true},
		{"unrelated", "audio_progress", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set(tinfoilEventsHeader, tc.value)
			}
			flags := parseTinfoilEventFlags(h)
			if flags.webSearch != tc.wantWS {
				t.Fatalf("webSearch = %v, want %v", flags.webSearch, tc.wantWS)
			}
			if flags.codeExecution != tc.wantCE {
				t.Fatalf("codeExecution = %v, want %v", flags.codeExecution, tc.wantCE)
			}
		})
	}
}

// TestTinfoilToolCallMarkerShape pins the on-the-wire format for code
// execution markers: the payload type is tinfoil.tool_call, and the
// tool field carries name + arguments on in_progress and name + output
// on completed.
func TestTinfoilToolCallMarkerShape(t *testing.T) {
	args := map[string]any{"command": "python3 script.py"}
	marker := tinfoilToolCallMarker("tc_1", "in_progress", "bash", args, "")
	if !strings.HasPrefix(marker, "\n"+tinfoilEventOpenTag) {
		t.Fatalf("marker must start with tag: got %q", marker)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\n"+tinfoilEventOpenTag), tinfoilEventCloseTag+"\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v (payload=%q)", err, payload)
	}
	if decoded["type"] != tinfoilEventToolCallType {
		t.Fatalf("payload.type = %v, want %q", decoded["type"], tinfoilEventToolCallType)
	}
	if decoded["item_id"] != "tc_1" {
		t.Fatalf("payload.item_id = %v, want tc_1", decoded["item_id"])
	}
	if decoded["status"] != "in_progress" {
		t.Fatalf("payload.status = %v, want in_progress", decoded["status"])
	}
	tool, _ := decoded["tool"].(map[string]any)
	if tool == nil || tool["name"] != "bash" {
		t.Fatalf("payload.tool.name malformed: %#v", decoded["tool"])
	}
	toolArgs, _ := tool["arguments"].(map[string]any)
	if toolArgs == nil || toolArgs["command"] != "python3 script.py" {
		t.Fatalf("payload.tool.arguments malformed: %#v", tool["arguments"])
	}
	if _, present := tool["output"]; present {
		t.Fatalf("in_progress marker must not include output")
	}
}

// TestTinfoilToolCallMarkerCompletedCarriesOutput pins that a completed
// marker has tool.output and no tool.arguments.
func TestTinfoilToolCallMarkerCompletedCarriesOutput(t *testing.T) {
	marker := tinfoilToolCallMarker("tc_1", "completed", "bash", nil, "Hello world\nexit_code: 0")
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\n"+tinfoilEventOpenTag), tinfoilEventCloseTag+"\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v", err)
	}
	tool, _ := decoded["tool"].(map[string]any)
	if tool == nil {
		t.Fatalf("payload.tool missing")
	}
	if tool["output"] != "Hello world\nexit_code: 0" {
		t.Fatalf("payload.tool.output = %v, want output text", tool["output"])
	}
	if _, present := tool["arguments"]; present {
		t.Fatalf("completed marker must not include arguments")
	}
}

// TestTinfoilToolCallMarkerTruncatesLargeOutput pins that the output
// field is truncated when it exceeds maxToolCallOutputInMarker.
func TestTinfoilToolCallMarkerTruncatesLargeOutput(t *testing.T) {
	longOutput := strings.Repeat("x", maxToolCallOutputInMarker+100)
	marker := tinfoilToolCallMarker("tc_1", "completed", "view", nil, longOutput)
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\n"+tinfoilEventOpenTag), tinfoilEventCloseTag+"\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v", err)
	}
	tool, _ := decoded["tool"].(map[string]any)
	output := tool["output"].(string)
	if len(output) > maxToolCallOutputInMarker+50 {
		t.Fatalf("output should be truncated, got len=%d", len(output))
	}
	if !strings.HasSuffix(output, "…[truncated]") {
		t.Fatalf("truncated output should end with truncation marker: %q", output[len(output)-20:])
	}
}

// TestTinfoilToolCallMarkersForRecords pins the non-streaming bulk
// marker builder for code-exec tools: each recorded tool call yields
// one in_progress + one terminal marker pair. Search and fetch records
// are skipped.
func TestTinfoilToolCallMarkersForRecords(t *testing.T) {
	records := []toolCallRecord{
		{name: "search", arguments: map[string]any{"query": "q"}}, // skipped
		{name: "bash", arguments: map[string]any{"command": "ls"}, output: "file.txt"},
		{name: "view", arguments: map[string]any{"path": "/tmp/x"}, output: "contents", errorReason: "tool_error"},
	}
	combined := tinfoilToolCallMarkersForRecords(records)
	// 2 code-exec records * 2 markers each = 4 markers
	if got := strings.Count(combined, tinfoilEventOpenTag); got != 4 {
		t.Fatalf("expected 4 markers (2 records * 2 phases), got %d in %q", got, combined)
	}
	if !strings.Contains(combined, `"tinfoil.tool_call"`) {
		t.Fatalf("markers must use tinfoil.tool_call type: %q", combined)
	}
	// Search records must be absent
	if strings.Contains(combined, `"search"`) {
		t.Fatalf("search records should be skipped: %q", combined)
	}
}

// TestCodeInterpreterCallEventShape pins the output item for Responses.
func TestCodeInterpreterCallEventShape(t *testing.T) {
	item := codeInterpreterCallEvent("ci_1", "completed", "bash", map[string]any{"command": "echo hi"}, "hi\n", "")
	if item["type"] != "code_interpreter_call" {
		t.Fatalf("type = %v, want code_interpreter_call", item["type"])
	}
	if item["status"] != "completed" {
		t.Fatalf("status = %v, want completed", item["status"])
	}
	code, _ := item["code"].(string)
	if !strings.Contains(code, "bash") {
		t.Fatalf("code should contain tool name: %q", code)
	}
	results, _ := item["results"].([]map[string]any)
	if len(results) != 1 || results[0]["text"] != "hi\n" {
		t.Fatalf("results malformed: %#v", item["results"])
	}
	if _, present := item["_tinfoil"]; present {
		t.Fatalf("successful call must omit _tinfoil sidecar")
	}
}

// TestCodeInterpreterCallEventBlocked pins that a blocked code-exec call
// collapses status onto failed (matching web_search_call behavior).
func TestCodeInterpreterCallEventBlocked(t *testing.T) {
	item := codeInterpreterCallEvent("ci_1", "blocked", "bash", nil, "", blockedToolErrorReason)
	if item["status"] != "failed" {
		t.Fatalf("blocked status must collapse to failed, got %v", item["status"])
	}
	sidecar, ok := item["_tinfoil"].(map[string]any)
	if !ok || sidecar["status"] != "blocked" {
		t.Fatalf("_tinfoil sidecar must preserve blocked status: %#v", item["_tinfoil"])
	}
}

// TestAttachResponsesCitationsPrependsCodeInterpreterCallItem pins that
// code-execution tool calls produce code_interpreter_call output items
// on the Responses path, prepended alongside web_search_call items.
func TestAttachResponsesCitationsPrependsCodeInterpreterCallItem(t *testing.T) {
	state := &citationState{nextIndex: 1}
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})
	tc.record(toolCallRecord{name: "bash", arguments: map[string]any{"command": "ls"}, output: "file.txt"})

	body := map[string]any{"output": []any{}}
	attachResponsesOutput(body, state, tc, false)

	items := body["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items (web_search_call + code_interpreter_call), got %d: %#v", len(items), items)
	}
	first := items[0].(map[string]any)
	if first["type"] != "web_search_call" {
		t.Fatalf("first item should be web_search_call, got %v", first["type"])
	}
	second := items[1].(map[string]any)
	if second["type"] != "code_interpreter_call" {
		t.Fatalf("second item should be code_interpreter_call, got %v", second["type"])
	}
	if second["status"] != "completed" {
		t.Fatalf("expected status=completed, got %v", second["status"])
	}
}

// TestAttachChatCitationsInjectsCodeExecMarkersWhenEnabled pins that
// code-execution markers are prepended to assistant content when the
// code_execution event flag is set.
func TestAttachChatCitationsInjectsCodeExecMarkersWhenEnabled(t *testing.T) {
	state := &citationState{nextIndex: 1}
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "bash", arguments: map[string]any{"command": "ls"}, output: "file.txt"})

	body := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": "Answer"},
			},
		},
	}

	attachChatOutput(body, state, tc, tinfoilEventFlags{codeExecution: true})

	message := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	content := message["content"].(string)
	if !strings.Contains(content, tinfoilEventToolCallType) {
		t.Fatalf("expected code-exec markers in content when code_execution enabled: %q", content)
	}
	if !strings.HasSuffix(content, "Answer") {
		t.Fatalf("original content should be preserved after markers: %q", content)
	}
}

// TestAttachChatCitationsSkipsCodeExecMarkersWhenDisabled pins that
// code-execution markers are NOT emitted when only webSearch is enabled.
func TestAttachChatCitationsSkipsCodeExecMarkersWhenDisabled(t *testing.T) {
	state := &citationState{nextIndex: 1}
	tc := &toolCallLog{}
	tc.record(toolCallRecord{name: "bash", arguments: map[string]any{"command": "ls"}, output: "file.txt"})

	body := map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"role": "assistant", "content": "Answer"}},
		},
	}

	attachChatOutput(body, state, tc, tinfoilEventFlags{webSearch: true})

	content := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if strings.Contains(content, tinfoilEventToolCallType) {
		t.Fatalf("code-exec markers must be absent when only webSearch enabled: %q", content)
	}
}

// countRunes is a small local helper so test assertions read as
// naturally as the production code that uses utf8.RuneCountInString.
func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
