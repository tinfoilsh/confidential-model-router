package citations

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

func newTestEmitter(sources ...Source) (*Emitter, *State) {
	state := &State{NextIndex: 1}
	state.Sources = append(state.Sources, sources...)
	return NewEmitter(state), state
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func TestEmitter_PlainTextPassesThrough(t *testing.T) {
	emitter, _ := newTestEmitter()
	content, annotations := emitter.Push("hello world")
	if content != "hello world" {
		t.Fatalf("content = %q, want 'hello world'", content)
	}
	if len(annotations) != 0 {
		t.Fatalf("annotations = %v, want none", annotations)
	}
	final, _ := emitter.Flush()
	if final != "" {
		t.Fatalf("final flush should be empty, got %q", final)
	}
}

func TestEmitter_HoldsBackUntilLinkCompletes(t *testing.T) {
	emitter, _ := newTestEmitter(Source{URL: "https://example.com", Title: "Example"})

	c1, a1 := emitter.Push("see [Example")
	if c1 != "see " {
		t.Fatalf("first content = %q, want 'see '", c1)
	}
	if len(a1) != 0 {
		t.Fatalf("unexpected annotations: %v", a1)
	}
	if emitter.Buffered() != len("[Example") {
		t.Fatalf("buffered = %d, want %d", emitter.Buffered(), len("[Example"))
	}

	c2, a2 := emitter.Push("](https://example.com) for details")
	wantContent := "[Example](https://example.com) for details"
	if c2 != wantContent {
		t.Fatalf("second content = %q, want %q", c2, wantContent)
	}
	if len(a2) != 1 {
		t.Fatalf("annotations = %v, want one", a2)
	}
	got := a2[0]
	if got.StartIndex != runeLen("see [") || got.EndIndex != runeLen("see [Example") {
		t.Fatalf("annotation span = (%d,%d), want (%d,%d)", got.StartIndex, got.EndIndex, runeLen("see ["), runeLen("see [Example"))
	}
	if got.Source.URL != "https://example.com" || got.Source.Title != "Example" {
		t.Fatalf("annotation source = %+v", got.Source)
	}
}

func TestEmitter_UnregisteredURLProducesNoAnnotation(t *testing.T) {
	emitter, _ := newTestEmitter()
	content, annotations := emitter.Push("[foo](https://unknown.example/doc)")
	if content != "[foo](https://unknown.example/doc)" {
		t.Fatalf("content = %q", content)
	}
	if len(annotations) != 0 {
		t.Fatalf("expected no annotations for unregistered URL, got %v", annotations)
	}
}

func TestEmitter_FullwidthBracketsNormalized(t *testing.T) {
	emitter, _ := newTestEmitter(Source{URL: "https://example.com/p", Title: "P"})
	content, annotations := emitter.Push("see \u3010Example\u3011(https://example.com/p) ok")
	want := "see [Example](https://example.com/p) ok"
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
	if len(annotations) != 1 {
		t.Fatalf("want exactly one annotation, got %v", annotations)
	}
	if annotations[0].StartIndex != runeLen("see [") || annotations[0].EndIndex != runeLen("see [Example") {
		t.Fatalf("annotation span = (%d,%d)", annotations[0].StartIndex, annotations[0].EndIndex)
	}
}

func TestEmitter_MixedBracketsNormalized(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"open fullwidth close ascii", "see \u3010Example](https://example.com/p) ok"},
		{"open ascii close fullwidth", "see [Example\u3011(https://example.com/p) ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emitter, _ := newTestEmitter(Source{URL: "https://example.com/p", Title: "P"})
			content, annotations := emitter.Push(tc.input)
			want := "see [Example](https://example.com/p) ok"
			if content != want {
				t.Fatalf("content = %q, want %q", content, want)
			}
			if len(annotations) != 1 {
				t.Fatalf("want exactly one annotation, got %v", annotations)
			}
			if annotations[0].StartIndex != runeLen("see [") || annotations[0].EndIndex != runeLen("see [Example") {
				t.Fatalf("annotation span = (%d,%d)", annotations[0].StartIndex, annotations[0].EndIndex)
			}
		})
	}
}

func TestEmitter_SplitAcrossManyChunks(t *testing.T) {
	emitter, _ := newTestEmitter(Source{URL: "https://example.com/a", Title: "A"})

	chunks := []string{"pre ", "[", "Exam", "ple](", "https://ex", "ample.com/a", ")", " tail"}
	var content string
	var annotations []Annotation
	for _, chunk := range chunks {
		c, a := emitter.Push(chunk)
		content += c
		annotations = append(annotations, a...)
	}
	finalContent, finalAnn := emitter.Flush()
	content += finalContent
	annotations = append(annotations, finalAnn...)

	wantContent := "pre [Example](https://example.com/a) tail"
	if content != wantContent {
		t.Fatalf("content = %q, want %q", content, wantContent)
	}
	if len(annotations) != 1 {
		t.Fatalf("annotations = %v, want one", annotations)
	}
	if annotations[0].StartIndex != runeLen("pre [") || annotations[0].EndIndex != runeLen("pre [Example") {
		t.Fatalf("annotation span = (%d,%d)", annotations[0].StartIndex, annotations[0].EndIndex)
	}
}

func TestEmitter_MultipleLinksBackToBack(t *testing.T) {
	emitter, _ := newTestEmitter(
		Source{URL: "https://a", Title: "A"},
		Source{URL: "https://b", Title: "B"},
	)
	content, annotations := emitter.Push("[A](https://a)[B](https://b) done")
	if content != "[A](https://a)[B](https://b) done" {
		t.Fatalf("content = %q", content)
	}
	if len(annotations) != 2 {
		t.Fatalf("annotations = %v, want two", annotations)
	}
	if annotations[0].Source.URL != "https://a" || annotations[1].Source.URL != "https://b" {
		t.Fatalf("annotation URLs = %q %q", annotations[0].Source.URL, annotations[1].Source.URL)
	}
}

func TestEmitter_OrphanOpenBracketFlushedAsPlain(t *testing.T) {
	emitter, _ := newTestEmitter()
	content1, _ := emitter.Push("a [b c d no link end")
	if content1 != "a " {
		t.Fatalf("content1 = %q, want 'a '", content1)
	}

	finalContent, finalAnn := emitter.Flush()
	want := "[b c d no link end"
	if finalContent != want {
		t.Fatalf("final content = %q, want %q", finalContent, want)
	}
	if len(finalAnn) != 0 {
		t.Fatalf("no annotations expected, got %v", finalAnn)
	}
}

func TestEmitter_CompletedNonLinkBracketFlushesOpener(t *testing.T) {
	emitter, _ := newTestEmitter()

	content, annotations := emitter.Push("see [note] nothing here")
	if annotations != nil {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	final, _ := emitter.Flush()
	got := content + final
	want := "see [note] nothing here"
	if got != want {
		t.Fatalf("combined content = %q, want %q", got, want)
	}
}

func TestEmitter_URLWithSpaceTreatedAsNonLink(t *testing.T) {
	emitter, _ := newTestEmitter()
	content, annotations := emitter.Push("[foo](not a url)")
	if len(annotations) != 0 {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	final, _ := emitter.Flush()
	got := content + final
	want := "[foo](not a url)"
	if got != want {
		t.Fatalf("combined content = %q, want %q", got, want)
	}
}

func TestEmitter_MaxBufferGivesUp(t *testing.T) {
	emitter, _ := newTestEmitter()
	buf := make([]rune, 0, MaxLinkBufferRunes+1)
	buf = append(buf, '[')
	for i := 0; i < MaxLinkBufferRunes; i++ {
		buf = append(buf, 'x')
	}
	content, annotations := emitter.Push(string(buf))
	if len(annotations) != 0 {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	if emitter.Buffered() > MaxLinkBufferRunes {
		t.Fatalf("buffered = %d after overflow, want <= %d", emitter.Buffered(), MaxLinkBufferRunes)
	}
	if !containsRune(content, '[') {
		t.Fatalf("expected `[` to be flushed when buffer overflows, got %q", content)
	}
}

func TestEmitter_FlushEmitsPending(t *testing.T) {
	emitter, _ := newTestEmitter()
	_, _ = emitter.Push("tail [partial")
	content, annotations := emitter.Flush()
	if annotations != nil {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	if content != "[partial" {
		t.Fatalf("flush content = %q, want '[partial'", content)
	}
}

func TestEmitter_PreservesTextAcrossMultiplePushes(t *testing.T) {
	emitter, _ := newTestEmitter(Source{URL: "https://example.com", Title: "Example"})
	emitter.Push("The sky is blue ")
	emitter.Push("[Example](https://example.com)")
	emitter.Push(" for details.")
	if got := string(emitter.Text()); got != "The sky is blue [Example](https://example.com) for details." {
		t.Fatalf("text = %q", got)
	}
}

func TestEmitter_HarmonyTokenResolvesToMarkdownLink(t *testing.T) {
	state := &State{NextIndex: 1, Harmony: true}
	state.Record("https://example.com/a", "Source A")
	emitter := NewEmitter(state)

	content, annotations := emitter.Push("claim\u30101\u2020L1-L3\u3011 done")
	final, finalAnn := emitter.Flush()
	got := content + final
	want := "claim[Source A](https://example.com/a) done"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	all := append(annotations, finalAnn...)
	if len(all) != 1 {
		t.Fatalf("annotations = %v, want one", all)
	}
	if all[0].StartIndex != runeLen("claim[") || all[0].EndIndex != runeLen("claim[Source A") {
		t.Fatalf("annotation span = (%d,%d)", all[0].StartIndex, all[0].EndIndex)
	}
	if all[0].Source.URL != "https://example.com/a" {
		t.Fatalf("annotation URL = %q", all[0].Source.URL)
	}
}

func TestEmitter_HarmonyBareCursorResolves(t *testing.T) {
	state := &State{NextIndex: 1, Harmony: true}
	state.Record("https://example.com/a", "Source A")
	emitter := NewEmitter(state)

	content, annotations := emitter.Push("see\u30101\u3011.")
	final, finalAnn := emitter.Flush()
	got := content + final
	want := "see[Source A](https://example.com/a)."
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	all := append(annotations, finalAnn...)
	if len(all) != 1 {
		t.Fatalf("annotations = %v, want one", all)
	}
}

func TestEmitter_HarmonyUnresolvedCursorPassesThrough(t *testing.T) {
	state := &State{NextIndex: 1, Harmony: true}
	state.Record("https://example.com/a", "Source A")
	emitter := NewEmitter(state)

	content, annotations := emitter.Push("claim\u301099\u3011 done")
	final, _ := emitter.Flush()
	got := content + final
	want := "claim\u301099\u3011 done"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if len(annotations) != 0 {
		t.Fatalf("expected no annotations, got %v", annotations)
	}
}

func TestEmitter_HarmonySplitAcrossChunks(t *testing.T) {
	state := &State{NextIndex: 1, Harmony: true}
	state.Record("https://example.com/a", "Source A")
	emitter := NewEmitter(state)

	chunks := []string{"pre ", "\u3010", "1", "\u2020L1", "-L3", "\u3011", " tail"}
	var content string
	var annotations []Annotation
	for _, chunk := range chunks {
		c, a := emitter.Push(chunk)
		content += c
		annotations = append(annotations, a...)
	}
	finalContent, finalAnn := emitter.Flush()
	content += finalContent
	annotations = append(annotations, finalAnn...)

	want := "pre [Source A](https://example.com/a) tail"
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
	if len(annotations) != 1 {
		t.Fatalf("annotations = %v, want one", annotations)
	}
	if annotations[0].StartIndex != runeLen("pre [") || annotations[0].EndIndex != runeLen("pre [Source A") {
		t.Fatalf("annotation span = (%d,%d)", annotations[0].StartIndex, annotations[0].EndIndex)
	}
}

func TestEmitter_HarmonyMultipleTokens(t *testing.T) {
	state := &State{NextIndex: 1, Harmony: true}
	state.Record("https://example.com/a", "Source A")
	state.Record("https://example.com/b", "Source B")
	emitter := NewEmitter(state)

	content, annotations := emitter.Push("first\u30101\u3011 second\u30102\u2020L4\u3011.")
	final, finalAnn := emitter.Flush()
	got := content + final
	want := "first[Source A](https://example.com/a) second[Source B](https://example.com/b)."
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	all := append(annotations, finalAnn...)
	if len(all) != 2 {
		t.Fatalf("annotations = %v, want two", all)
	}
	if all[0].Source.URL != "https://example.com/a" || all[1].Source.URL != "https://example.com/b" {
		t.Fatalf("annotation URLs = %q, %q", all[0].Source.URL, all[1].Source.URL)
	}
}

func TestEmitter_HarmonyDisabledLeavesTokensUntouched(t *testing.T) {
	state := &State{NextIndex: 1}
	state.Record("https://example.com/a", "Source A")
	emitter := NewEmitter(state)

	content, annotations := emitter.Push("claim\u30101\u2020L1\u3011.")
	final, _ := emitter.Flush()
	got := content + final
	want := "claim\u30101\u2020L1\u3011."
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if len(annotations) != 0 {
		t.Fatalf("expected no annotations, got %v", annotations)
	}
}

func TestFindNextLinkOpen(t *testing.T) {
	runes := []rune("abc[de\u3010fg")
	idx, ok := findNextLinkOpen(runes, 0)
	if !ok || idx != 3 {
		t.Fatalf("idx=%d ok=%v, want 3,true", idx, ok)
	}
	idx, ok = findNextLinkOpen(runes, 4)
	if !ok || idx != 6 {
		t.Fatalf("idx=%d ok=%v, want 6,true", idx, ok)
	}
	if _, ok := findNextLinkOpen([]rune("nothing here"), 0); ok {
		t.Fatal("expected no open bracket")
	}
}

func TestMatchMarkdownLink(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		start        int
		wantOutcome  LinkMatchOutcome
		wantEnd      int
		wantLabelSt  int
		wantLabelEnd int
		wantURL      string
	}{
		{"complete ascii", "[foo](https://example.com)", 0, LinkMatch, 26, 1, 4, "https://example.com"},
		{"complete fullwidth", "\u3010foo\u3011(https://example.com)", 0, LinkMatch, 26, 1, 4, "https://example.com"},
		{"complete mixed open-fullwidth close-ascii", "\u3010foo](https://example.com)", 0, LinkMatch, 26, 1, 4, "https://example.com"},
		{"complete mixed open-ascii close-fullwidth", "[foo\u3011(https://example.com)", 0, LinkMatch, 26, 1, 4, "https://example.com"},
		{"incomplete no close bracket", "[foo", 0, LinkIncomplete, 0, 0, 0, ""},
		{"incomplete no open paren", "[foo]", 0, LinkIncomplete, 0, 0, 0, ""},
		{"incomplete no close paren", "[foo](https://example.com", 0, LinkIncomplete, 0, 0, 0, ""},
		{"invalid missing paren after label", "[foo] bar", 0, LinkInvalid, 0, 0, 0, ""},
		{"invalid empty label", "[](https://a.example)", 0, LinkInvalid, 0, 0, 0, ""},
		{"invalid unsupported scheme", "[foo](ftp://example.com)", 0, LinkInvalid, 0, 0, 0, ""},
		{"invalid url with space", "[foo](not a url)", 0, LinkInvalid, 0, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runes := []rune(tc.input)
			outcome, end, labelStart, labelEnd, url := MatchMarkdownLink(runes, tc.start)
			if outcome != tc.wantOutcome {
				t.Fatalf("outcome=%d, want %d", outcome, tc.wantOutcome)
			}
			if outcome != LinkMatch {
				return
			}
			if end != tc.wantEnd || labelStart != tc.wantLabelSt || labelEnd != tc.wantLabelEnd || url != tc.wantURL {
				t.Fatalf("got (end=%d, lstart=%d, lend=%d, url=%q), want (%d,%d,%d,%q)",
					end, labelStart, labelEnd, url,
					tc.wantEnd, tc.wantLabelSt, tc.wantLabelEnd, tc.wantURL)
			}
		})
	}
}

func TestResolveSourcePrefersTitled(t *testing.T) {
	state := &State{
		NextIndex: 1,
		Sources: []Source{
			{URL: "https://example.com", Title: ""},
			{URL: "https://example.com", Title: "Titled"},
		},
	}
	source, ok := state.ResolveSource("https://example.com")
	if !ok {
		t.Fatal("expected ResolveSource to find the URL")
	}
	if source.Title != "Titled" {
		t.Fatalf("title = %q, want Titled", source.Title)
	}
}

func containsRune(s string, r rune) bool {
	for _, ch := range s {
		if ch == r {
			return true
		}
	}
	return false
}

var _ = reflect.TypeOf(Annotation{})
