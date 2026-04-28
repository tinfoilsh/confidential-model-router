package citations

import "strings"

// MaxLinkBufferRunes caps how many runes we will hold back waiting for a
// markdown link to complete before giving up and flushing the opening bracket
// as plain content.
const MaxLinkBufferRunes = 4096

// Annotation is a rune-indexed source span emitted by the streaming emitter.
// It mirrors the fields the router already attaches at the end of a
// non-streaming tool loop so the chat and responses wire-format layers can
// translate it into the two documented OpenAI shapes without duplicating
// index logic.
type Annotation struct {
	StartIndex int
	EndIndex   int
	Source     Source
}

// Emitter is the streaming counterpart of the non-streaming citation
// attachment functions. As the upstream model emits content deltas, the
// emitter holds back only the tail that could be part of an in-progress
// markdown link, flushing everything upstream of any opening bracket as a
// plain content delta immediately. When a `[label](url)` (or fullwidth
// `【label】(url)`) span completes and the URL matches a recorded source,
// the emitter produces an Annotation whose start/end span the label in
// rune space.
//
// The emitter is NOT safe for concurrent use.
type Emitter struct {
	state *State

	// text is the full normalized text in rune form. It grows on every
	// push call and is never truncated.
	text []rune

	// flushedRunes tracks how many runes of `text` have been emitted.
	flushedRunes int
}

// NewEmitter constructs an emitter that resolves inline markdown links
// against the sources the given citation state has accumulated.
func NewEmitter(state *State) *Emitter {
	return &Emitter{state: state}
}

// Push appends an incoming content delta and returns the content chunk and
// annotation chunk that should be forwarded to the client.
func (e *Emitter) Push(delta string) (contentChunk string, annotations []Annotation) {
	if delta == "" {
		return "", nil
	}
	for _, r := range delta {
		e.text = append(e.text, r)
	}
	return e.drain(false)
}

// Flush drains whatever content is still buffered, unconditionally.
func (e *Emitter) Flush() (contentChunk string, annotations []Annotation) {
	return e.drain(true)
}

// Text returns the full accumulated text for test inspection.
func (e *Emitter) Text() []rune { return e.text }

// Buffered reports how many runes are still held by the link-completion buffer.
func (e *Emitter) Buffered() int { return len(e.text) - e.flushedRunes }

// Match links. On LinkOpen, keep adding to buffer until LinkClosed or failure.
func (e *Emitter) drain(final bool) (string, []Annotation) {
	e.resolveHarmonyTokens()

	var (
		contentBuilder strings.Builder
		annotations    []Annotation
	)

	for e.flushedRunes < len(e.text) {
		openIndex, ok := findNextLinkOpen(e.text, e.flushedRunes)
		if !ok {
			contentBuilder.WriteString(string(e.text[e.flushedRunes:]))
			e.flushedRunes = len(e.text)
			break
		}

		if openIndex > e.flushedRunes {
			contentBuilder.WriteString(string(e.text[e.flushedRunes:openIndex]))
			e.flushedRunes = openIndex
		}

		outcome, endIndex, labelStart, labelEnd, url := MatchMarkdownLink(e.text, openIndex)
		switch outcome {
		case LinkMatch:
			NormalizeLinkBrackets(e.text, openIndex, endIndex)
			contentBuilder.WriteString(string(e.text[openIndex:endIndex]))
			if source, sourceOK := e.state.ResolveSource(url); sourceOK {
				source.URL = url
				annotations = append(annotations, Annotation{
					StartIndex: labelStart,
					EndIndex:   labelEnd,
					Source:     source,
				})
			}
			e.flushedRunes = endIndex
		case LinkIncomplete:
			if final || (len(e.text)-openIndex) > MaxLinkBufferRunes {
				contentBuilder.WriteRune(e.text[openIndex])
				e.flushedRunes = openIndex + 1
				continue
			}
			return contentBuilder.String(), annotations
		case LinkInvalid:
			contentBuilder.WriteRune(e.text[openIndex])
			e.flushedRunes = openIndex + 1
		}
	}

	return contentBuilder.String(), annotations
}

// resolveHarmonyTokens rewrites complete Harmony-format citations (【N†L...】)
// in the unflushed buffer suffix into ASCII markdown links. The regular
// link-match path then picks them up and emits annotations.
func (e *Emitter) resolveHarmonyTokens() {
	if e.state == nil || !e.state.Harmony || e.flushedRunes >= len(e.text) {
		return
	}
	suffix := string(e.text[e.flushedRunes:])
	resolved := e.state.ResolveHarmonyCitations(suffix)
	if resolved == suffix {
		return
	}
	e.text = append(e.text[:e.flushedRunes], []rune(resolved)...)
}

// findNextLinkOpen returns the index of the next opening bracket that could
// start a markdown link at or after `from`.
func findNextLinkOpen(text []rune, from int) (int, bool) {
	for i := from; i < len(text); i++ {
		r := text[i]
		if r == '[' || r == '\u3010' {
			return i, true
		}
	}
	return 0, false
}

// LinkMatchOutcome describes the result of attempting to match a markdown link.
type LinkMatchOutcome int

const (
	LinkMatch      LinkMatchOutcome = iota
	LinkIncomplete                  // could still grow into a match
	LinkInvalid                     // definitely not a link
)

// MatchMarkdownLink examines the rune slice starting at start (which must
// point at `[` or `【`) and reports whether it contains a complete markdown link.
func MatchMarkdownLink(text []rune, start int) (outcome LinkMatchOutcome, end, labelStart, labelEnd int, url string) {
	if start >= len(text) {
		return LinkIncomplete, 0, 0, 0, ""
	}
	open := text[start]
	switch open {
	case '[', '\u3010':
	default:
		return LinkInvalid, 0, 0, 0, ""
	}

	labelStart = start + 1
	labelEnd = -1
	for i := labelStart; i < len(text); i++ {
		if text[i] == ']' || text[i] == '\u3011' {
			labelEnd = i
			break
		}
	}
	if labelEnd < 0 {
		return LinkIncomplete, 0, 0, 0, ""
	}
	if labelEnd == labelStart {
		return LinkInvalid, 0, 0, 0, ""
	}

	parenIdx := labelEnd + 1
	if parenIdx >= len(text) {
		return LinkIncomplete, 0, 0, 0, ""
	}
	if text[parenIdx] != '(' {
		return LinkInvalid, 0, 0, 0, ""
	}

	urlStart := parenIdx + 1
	urlEnd := -1
	for i := urlStart; i < len(text); i++ {
		r := text[i]
		if r == ')' {
			urlEnd = i
			break
		}
		if isSpaceRune(r) {
			return LinkInvalid, 0, 0, 0, ""
		}
	}
	if urlEnd < 0 {
		return LinkIncomplete, 0, 0, 0, ""
	}

	url = string(text[urlStart:urlEnd])
	if !IsSupportedURL(url) {
		return LinkInvalid, 0, 0, 0, ""
	}

	return LinkMatch, urlEnd + 1, labelStart, labelEnd, url
}

// NormalizeLinkBrackets rewrites fullwidth brackets to ASCII in place.
func NormalizeLinkBrackets(text []rune, start, end int) {
	if start < 0 || end > len(text) || start >= end {
		return
	}
	if text[start] == '\u3010' {
		text[start] = '['
	}
	for i := start + 1; i < end; i++ {
		if text[i] == '\u3011' {
			text[i] = ']'
			break
		}
	}
}

func isSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// IsSupportedURL reports whether a URL string has a supported scheme.
func IsSupportedURL(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}
