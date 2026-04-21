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
	marker := tinfoilEventMarker("ws_1", "in_progress", map[string]any{"type": "search", "query": "q"}, "")
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
	marker := tinfoilEventMarker("ws_1", "blocked", map[string]any{"type": "search"}, "safety policy rejected this query")
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
	state.recordToolCall(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

	original := "A claim [Example page](https://example.com/page) stands."
	body := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": original},
			},
		},
	}

	attachChatCitations(body, state, true)

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
	state.recordToolCall(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})
	body := map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"role": "assistant", "content": "Answer"}},
		},
	}

	attachChatCitations(body, state, false)

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
	state.recordToolCall(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

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

	attachResponsesCitations(body, state, false)

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
	state.recordToolCall(toolCallRecord{
		name:        "search",
		arguments:   map[string]any{"query": "sensitive"},
		errorReason: blockedToolErrorReason,
	})

	body := map[string]any{
		"output": []any{},
	}

	attachResponsesCitations(body, state, false)

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
	state.recordToolCall(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

	body := map[string]any{"output": []any{}}
	attachResponsesCitations(body, state, false)

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
	state.recordToolCall(toolCallRecord{name: "search", arguments: map[string]any{"query": "q"}})

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

	attachResponsesCitations(body, state, false)

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

// countRunes is a small local helper so test assertions read as
// naturally as the production code that uses utf8.RuneCountInString.
func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
