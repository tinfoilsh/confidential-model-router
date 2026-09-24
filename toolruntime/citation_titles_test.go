package toolruntime

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/toolruntime/citations"
)

func TestCitationTitles(t *testing.T) {
	const sourceURL = "https://example.com/article"
	const content = "See [source](" + sourceURL + ") for details."
	cases := []struct {
		name   string
		titles []string
		want   string
	}{
		{name: "empty", titles: []string{""}, want: sourceURL},
		{name: "whitespace", titles: []string{" \t\n"}, want: sourceURL},
		{name: "titled", titles: []string{"Article title"}, want: "Article title"},
		{name: "unicode", titles: []string{"Résumé 日本語"}, want: "Résumé 日本語"},
		{name: "preserve title", titles: []string{" Article title "}, want: " Article title "},
		{name: "prefer real title", titles: []string{"", "Article title"}, want: "Article title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &citations.State{}
			for _, title := range tc.titles {
				state.Record(sourceURL, title)
			}
			t.Run("chat response", func(t *testing.T) {
				annotations := state.NestedAnnotationsFor(content)
				if len(annotations) != 1 {
					t.Fatalf("expected one citation, got %#v", annotations)
				}
				citation := annotations[0].(map[string]any)["url_citation"].(map[string]any)
				assertCitationTitle(t, citation, tc.want, sourceURL)
			})
			t.Run("responses response", func(t *testing.T) {
				annotations := state.FlatAnnotationsFor(content)
				if len(annotations) != 1 {
					t.Fatalf("expected one citation, got %#v", annotations)
				}
				assertCitationTitle(t, annotations[0].(map[string]any), tc.want, sourceURL)
			})
			t.Run("chat stream", func(t *testing.T) {
				streamer, rec := newTestChatStreamer(t)
				streamer.citations = state
				streamer.emitter = citations.NewEmitter(state)
				streamer.emitContentDelta("See [source](https://example.com/")
				streamer.emitContentDelta("article) for details.")
				streamer.flushCitations()
				var annotations []any
				for _, event := range citationStreamEvents(t, rec.Body.String()) {
					for _, rawChoice := range event["choices"].([]any) {
						delta := rawChoice.(map[string]any)["delta"].(map[string]any)
						if values, ok := delta["annotations"].([]any); ok {
							annotations = append(annotations, values...)
						}
					}
				}
				if len(annotations) != 1 {
					t.Fatalf("expected one streamed citation, got %#v", annotations)
				}
				citation := annotations[0].(map[string]any)["url_citation"].(map[string]any)
				assertCitationTitle(t, citation, tc.want, sourceURL)
			})
			t.Run("responses stream", func(t *testing.T) {
				streamer, rec := newTestResponsesStreamer(t)
				streamer.citations = state
				streamer.outputIndexMap[0] = 0
				streamer.handleOutputTextDelta(map[string]any{
					"output_index":  float64(0),
					"item_id":       "msg_1",
					"content_index": float64(0),
					"delta":         content,
				})
				var annotations []map[string]any
				for _, event := range citationStreamEvents(t, rec.Body.String()) {
					if event["type"] == "response.output_text.annotation.added" {
						annotations = append(annotations, event["annotation"].(map[string]any))
					}
				}
				if len(annotations) != 1 {
					t.Fatalf("expected one streamed citation, got %#v", annotations)
				}
				assertCitationTitle(t, annotations[0], tc.want, sourceURL)
			})
			for i, source := range state.Sources {
				if source.Title != tc.titles[i] {
					t.Fatalf("serialization mutated source title: %q", source.Title)
				}
			}
		})
	}
}

func assertCitationTitle(t *testing.T, citation map[string]any, wantTitle, wantURL string) {
	t.Helper()
	if title, ok := citation["title"].(string); !ok || title != wantTitle {
		t.Errorf("citation title = %#v, want string %q", citation["title"], wantTitle)
	}
	if citation["url"] != wantURL {
		t.Errorf("citation URL = %#v, want %q", citation["url"], wantURL)
	}
}

func citationStreamEvents(t *testing.T, wire string) []map[string]any {
	t.Helper()
	reader := newSSEReader(strings.NewReader(wire))
	var events []map[string]any
	for {
		frame, err := reader.next()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(frame.data), &event); err != nil {
			t.Fatalf("invalid SSE JSON: %v", err)
		}
		events = append(events, event)
	}
}
