package toolruntime

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

func newTestEmitter(sources ...citationSource) (*citationEmitter, *citationState) {
	state := &citationState{nextIndex: 1}
	state.sources = append(state.sources, sources...)
	return newCitationEmitter(state), state
}

// runeLen is a test-only helper for building span assertions without
// depending on unicode/utf8 at every call site.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// buffered reports how many runes are still held by the emitter's
// link-completion buffer. Used only by tests to assert hold-back behavior.
func (e *citationEmitter) buffered() int { return len(e.text) - e.flushedRunes }

func TestCitationEmitter_PlainTextPassesThrough(t *testing.T) {
	emitter, _ := newTestEmitter()
	content, annotations := emitter.push("hello world")
	if content != "hello world" {
		t.Fatalf("content = %q, want 'hello world'", content)
	}
	if len(annotations) != 0 {
		t.Fatalf("annotations = %v, want none", annotations)
	}
	final, _ := emitter.flush()
	if final != "" {
		t.Fatalf("final flush should be empty, got %q", final)
	}
}

func TestCitationEmitter_HoldsBackUntilLinkCompletes(t *testing.T) {
	emitter, _ := newTestEmitter(citationSource{url: "https://example.com", title: "Example"})

	c1, a1 := emitter.push("see [Example")
	if c1 != "see " {
		t.Fatalf("first content = %q, want 'see '", c1)
	}
	if len(a1) != 0 {
		t.Fatalf("unexpected annotations: %v", a1)
	}
	if emitter.buffered() != len("[Example") {
		t.Fatalf("buffered = %d, want %d", emitter.buffered(), len("[Example"))
	}

	c2, a2 := emitter.push("](https://example.com) for details")
	wantContent := "[Example](https://example.com) for details"
	if c2 != wantContent {
		t.Fatalf("second content = %q, want %q", c2, wantContent)
	}
	if len(a2) != 1 {
		t.Fatalf("annotations = %v, want one", a2)
	}
	got := a2[0]
	if got.startIndex != runeLen("see [") || got.endIndex != runeLen("see [Example") {
		t.Fatalf("annotation span = (%d,%d), want (%d,%d)", got.startIndex, got.endIndex, runeLen("see ["), runeLen("see [Example"))
	}
	if got.source.url != "https://example.com" || got.source.title != "Example" {
		t.Fatalf("annotation source = %+v", got.source)
	}
}

func TestCitationEmitter_UnregisteredURLProducesNoAnnotation(t *testing.T) {
	emitter, _ := newTestEmitter()
	content, annotations := emitter.push("[foo](https://unknown.example/doc)")
	if content != "[foo](https://unknown.example/doc)" {
		t.Fatalf("content = %q", content)
	}
	if len(annotations) != 0 {
		t.Fatalf("expected no annotations for unregistered URL, got %v", annotations)
	}
}

func TestCitationEmitter_FullwidthBracketsNormalized(t *testing.T) {
	emitter, _ := newTestEmitter(citationSource{url: "https://example.com/p", title: "P"})
	content, annotations := emitter.push("see \u3010Example\u3011(https://example.com/p) ok")
	want := "see [Example](https://example.com/p) ok"
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
	if len(annotations) != 1 {
		t.Fatalf("want exactly one annotation, got %v", annotations)
	}
	if annotations[0].startIndex != runeLen("see [") || annotations[0].endIndex != runeLen("see [Example") {
		t.Fatalf("annotation span = (%d,%d)", annotations[0].startIndex, annotations[0].endIndex)
	}
}

// gpt-oss has been observed emitting a mixed-bracket shape where the opening
// lenticular fullwidth bracket is paired with an ASCII close bracket, for
// example `【Example](https://example.com/p)`. Verify the emitter recovers
// the citation in both mixed permutations so the annotation count matches
// what the user visually sees.
func TestCitationEmitter_MixedBracketsNormalized(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"open fullwidth close ascii", "see \u3010Example](https://example.com/p) ok"},
		{"open ascii close fullwidth", "see [Example\u3011(https://example.com/p) ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emitter, _ := newTestEmitter(citationSource{url: "https://example.com/p", title: "P"})
			content, annotations := emitter.push(tc.input)
			want := "see [Example](https://example.com/p) ok"
			if content != want {
				t.Fatalf("content = %q, want %q", content, want)
			}
			if len(annotations) != 1 {
				t.Fatalf("want exactly one annotation, got %v", annotations)
			}
			if annotations[0].startIndex != runeLen("see [") || annotations[0].endIndex != runeLen("see [Example") {
				t.Fatalf("annotation span = (%d,%d)", annotations[0].startIndex, annotations[0].endIndex)
			}
		})
	}
}

func TestCitationEmitter_SplitAcrossManyChunks(t *testing.T) {
	emitter, _ := newTestEmitter(citationSource{url: "https://example.com/a", title: "A"})

	chunks := []string{"pre ", "[", "Exam", "ple](", "https://ex", "ample.com/a", ")", " tail"}
	var content string
	var annotations []citationAnnotation
	for _, chunk := range chunks {
		c, a := emitter.push(chunk)
		content += c
		annotations = append(annotations, a...)
	}
	finalContent, finalAnn := emitter.flush()
	content += finalContent
	annotations = append(annotations, finalAnn...)

	wantContent := "pre [Example](https://example.com/a) tail"
	if content != wantContent {
		t.Fatalf("content = %q, want %q", content, wantContent)
	}
	if len(annotations) != 1 {
		t.Fatalf("annotations = %v, want one", annotations)
	}
	if annotations[0].startIndex != runeLen("pre [") || annotations[0].endIndex != runeLen("pre [Example") {
		t.Fatalf("annotation span = (%d,%d)", annotations[0].startIndex, annotations[0].endIndex)
	}
}

func TestCitationEmitter_MultipleLinksBackToBack(t *testing.T) {
	emitter, _ := newTestEmitter(
		citationSource{url: "https://a", title: "A"},
		citationSource{url: "https://b", title: "B"},
	)
	content, annotations := emitter.push("[A](https://a)[B](https://b) done")
	if content != "[A](https://a)[B](https://b) done" {
		t.Fatalf("content = %q", content)
	}
	if len(annotations) != 2 {
		t.Fatalf("annotations = %v, want two", annotations)
	}
	if annotations[0].source.url != "https://a" || annotations[1].source.url != "https://b" {
		t.Fatalf("annotation URLs = %q %q", annotations[0].source.url, annotations[1].source.url)
	}
}

func TestCitationEmitter_OrphanOpenBracketFlushedAsPlain(t *testing.T) {
	emitter, _ := newTestEmitter()
	content1, _ := emitter.push("a [b c d no link end")
	// Open bracket is buffered; the rest of the text up to `[` has flushed.
	if content1 != "a " {
		t.Fatalf("content1 = %q, want 'a '", content1)
	}

	finalContent, finalAnn := emitter.flush()
	want := "[b c d no link end"
	if finalContent != want {
		t.Fatalf("final content = %q, want %q", finalContent, want)
	}
	if len(finalAnn) != 0 {
		t.Fatalf("no annotations expected, got %v", finalAnn)
	}
}

func TestCitationEmitter_CompletedNonLinkBracketFlushesOpener(t *testing.T) {
	emitter, _ := newTestEmitter()

	content, annotations := emitter.push("see [note] nothing here")
	if annotations != nil {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	// At `[note]` with a following space, the emitter knows it's not a link.
	// It flushes only the opening bracket per scan iteration, which forces
	// it to re-scan the rest of the buffer. The final content we receive
	// should reconstruct the original text exactly.
	final, _ := emitter.flush()
	got := content + final
	want := "see [note] nothing here"
	if got != want {
		t.Fatalf("combined content = %q, want %q", got, want)
	}
}

func TestCitationEmitter_URLWithSpaceTreatedAsNonLink(t *testing.T) {
	emitter, _ := newTestEmitter()
	content, annotations := emitter.push("[foo](not a url)")
	if len(annotations) != 0 {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	// The content should still contain every rune, even though the `[` got
	// flushed piece by piece because matchMarkdownLink rejected it.
	final, _ := emitter.flush()
	got := content + final
	want := "[foo](not a url)"
	if got != want {
		t.Fatalf("combined content = %q, want %q", got, want)
	}
}

func TestCitationEmitter_MaxBufferGivesUp(t *testing.T) {
	emitter, _ := newTestEmitter()
	buf := make([]rune, 0, maxLinkBufferRunes+1)
	buf = append(buf, '[')
	for i := 0; i < maxLinkBufferRunes; i++ {
		buf = append(buf, 'x')
	}
	content, annotations := emitter.push(string(buf))
	if len(annotations) != 0 {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	// On overflow, the opener should have been flushed even without a
	// closing bracket, so the buffered portion is at most one less than the
	// overflowed rune count.
	if emitter.buffered() > maxLinkBufferRunes {
		t.Fatalf("buffered = %d after overflow, want <= %d", emitter.buffered(), maxLinkBufferRunes)
	}
	if !containsRune(content, '[') {
		t.Fatalf("expected `[` to be flushed when buffer overflows, got %q", content)
	}
}

func TestCitationEmitter_FlushEmitsPending(t *testing.T) {
	emitter, _ := newTestEmitter()
	_, _ = emitter.push("tail [partial")
	content, annotations := emitter.flush()
	if annotations != nil {
		t.Fatalf("unexpected annotations: %v", annotations)
	}
	if content != "[partial" {
		t.Fatalf("flush content = %q, want '[partial'", content)
	}
}

func TestCitationEmitter_PreservesTextAcrossMultiplePushes(t *testing.T) {
	emitter, _ := newTestEmitter(citationSource{url: "https://example.com", title: "Example"})
	emitter.push("The sky is blue ")
	emitter.push("[Example](https://example.com)")
	emitter.push(" for details.")
	if got := string(emitter.text); got != "The sky is blue [Example](https://example.com) for details." {
		t.Fatalf("text = %q", got)
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
		wantOutcome  linkMatchOutcome
		wantEnd      int
		wantLabelSt  int
		wantLabelEnd int
		wantURL      string
	}{
		{"complete ascii", "[foo](https://example.com)", 0, linkMatch, 26, 1, 4, "https://example.com"},
		{"complete fullwidth", "\u3010foo\u3011(https://example.com)", 0, linkMatch, 26, 1, 4, "https://example.com"},
		{"complete mixed open-fullwidth close-ascii", "\u3010foo](https://example.com)", 0, linkMatch, 26, 1, 4, "https://example.com"},
		{"complete mixed open-ascii close-fullwidth", "[foo\u3011(https://example.com)", 0, linkMatch, 26, 1, 4, "https://example.com"},
		{"incomplete no close bracket", "[foo", 0, linkIncomplete, 0, 0, 0, ""},
		{"incomplete no open paren", "[foo]", 0, linkIncomplete, 0, 0, 0, ""},
		{"incomplete no close paren", "[foo](https://example.com", 0, linkIncomplete, 0, 0, 0, ""},
		{"invalid missing paren after label", "[foo] bar", 0, linkInvalid, 0, 0, 0, ""},
		{"invalid empty label", "[](https://a.example)", 0, linkInvalid, 0, 0, 0, ""},
		{"invalid unsupported scheme", "[foo](ftp://example.com)", 0, linkInvalid, 0, 0, 0, ""},
		{"invalid url with space", "[foo](not a url)", 0, linkInvalid, 0, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runes := []rune(tc.input)
			outcome, end, labelStart, labelEnd, url := matchMarkdownLink(runes, tc.start)
			if outcome != tc.wantOutcome {
				t.Fatalf("outcome=%d, want %d", outcome, tc.wantOutcome)
			}
			if outcome != linkMatch {
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
	state := &citationState{
		nextIndex: 1,
		sources: []citationSource{
			{url: "https://example.com", title: ""},
			{url: "https://example.com", title: "Titled"},
		},
	}
	source, ok := state.resolveSource("https://example.com")
	if !ok {
		t.Fatal("expected resolveSource to find the URL")
	}
	if source.title != "Titled" {
		t.Fatalf("title = %q, want Titled", source.title)
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

// Guard against accidental changes to the signature of citationAnnotation
// since the wire-format layers pattern-match on it.
var _ = reflect.TypeOf(citationAnnotation{})
