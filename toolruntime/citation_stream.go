package toolruntime

import "strings"

// maxLinkBufferRunes caps how many runes we will hold back waiting for a
// markdown link to complete before giving up and flushing the opening bracket
// as plain content. Models never emit multi-kilobyte labels, so the cap is a
// defense against a pathological stream (e.g. a stray `[` with no matching
// `]`) rather than a correctness mechanism.
const maxLinkBufferRunes = 4096

// citationAnnotation is a rune-indexed source span emitted by the citation
// emitter. It mirrors the fields the router already attaches at the end of a
// non-streaming tool loop so the chat and responses wire-format layers can
// translate it into the two documented OpenAI shapes (nested url_citation and
// flat url_citation) without duplicating index logic.
type citationAnnotation struct {
	startIndex int
	endIndex   int
	source     citationSource
}

// citationEmitter is the streaming counterpart of `attachChatCitations` /
// `attachResponsesCitations`. As the upstream model emits content deltas, the
// emitter holds back only the tail that could be part of an in-progress
// markdown link, flushing everything upstream of any opening bracket as a
// plain content delta immediately. When a `[label](url)` (or fullwidth
// `\u3010label\u3011(url)`) span completes and the URL matches a source the
// router recorded during tool execution, the emitter produces a
// citationAnnotation whose start/end span the label in rune space -- matching
// the semantics of the existing `matchesFor` post-processor so wire-format
// adapters do not need to know whether a given annotation was built inline or
// at the end of the turn.
//
// The emitter is NOT safe for concurrent use. A single streaming turn owns
// one emitter for the full duration of the stream.
type citationEmitter struct {
	state *citationState

	// text is the full normalized text the caller has committed to in rune
	// form. It grows on every push call and is never truncated; it is the
	// source of truth for rune-index annotations.
	text []rune

	// flushedRunes tracks how many runes of `text` have been emitted to the
	// client. Everything in text[flushedRunes:] is still buffered.
	flushedRunes int
}

// newCitationEmitter constructs an emitter that resolves inline markdown
// links against the sources the given citation state has accumulated during
// the tool loop. The state pointer is borrowed, not copied, so sources that
// are registered after the emitter was constructed still resolve correctly
// if they arrive before the link completes in the text stream.
func newCitationEmitter(state *citationState) *citationEmitter {
	return &citationEmitter{state: state}
}

// push appends an incoming content delta to the emitter's buffer and returns
// the content chunk (if any) and the annotation chunk (if any) that should
// be forwarded to the client. Callers are expected to emit contentChunk
// before annotations, matching the order clients index text by.
//
// Both return values are empty when the incoming delta was entirely absorbed
// into the link-hold buffer, which is the correct behaviour: the caller must
// not forward a truncated `[label](url` span because downstream UIs would
// render it as literal text.
func (e *citationEmitter) push(delta string) (contentChunk string, annotations []citationAnnotation) {
	if delta == "" {
		return "", nil
	}
	for _, r := range delta {
		e.text = append(e.text, r)
	}
	return e.drain(false)
}

// flush drains whatever content is still buffered, unconditionally. It is
// called at end-of-stream so a buffered opening bracket with no closing match
// does not get dropped on the floor.
func (e *citationEmitter) flush() (contentChunk string, annotations []citationAnnotation) {
	return e.drain(true)
}

// drain is the shared scan loop used by push (final=false) and flush
// (final=true). When final is true we give up on any in-progress link that
// has not closed and emit its prefix as plain content so the user still sees
// every rune the model produced.
func (e *citationEmitter) drain(final bool) (string, []citationAnnotation) {
	var (
		contentBuilder strings.Builder
		annotations    []citationAnnotation
	)

	for e.flushedRunes < len(e.text) {
		openIndex, ok := findNextLinkOpen(e.text, e.flushedRunes)
		if !ok {
			// No opening bracket anywhere ahead -- everything is plain content.
			contentBuilder.WriteString(string(e.text[e.flushedRunes:]))
			e.flushedRunes = len(e.text)
			break
		}

		// Emit everything up to the opening bracket as plain content; it is
		// guaranteed not to contain a link-initiating character so it is safe
		// to commit without further inspection.
		if openIndex > e.flushedRunes {
			contentBuilder.WriteString(string(e.text[e.flushedRunes:openIndex]))
			e.flushedRunes = openIndex
		}

		outcome, endIndex, labelStart, labelEnd, url := matchMarkdownLink(e.text, openIndex)
		switch outcome {
		case linkMatch:
			// Normalize fullwidth brackets in place so `text` always reflects
			// the ASCII form that downstream consumers (SDKs, UIs) receive.
			normalizeLinkBrackets(e.text, openIndex, endIndex)
			contentBuilder.WriteString(string(e.text[openIndex:endIndex]))
			if source, sourceOK := e.state.resolveSource(url); sourceOK {
				annotations = append(annotations, citationAnnotation{
					startIndex: labelStart,
					endIndex:   labelEnd,
					source:     source,
				})
			}
			e.flushedRunes = endIndex
		case linkIncomplete:
			if final || (len(e.text)-openIndex) > maxLinkBufferRunes {
				// Give up: flush the single opening bracket as plain content
				// and resume scanning from the rune after it. This keeps the
				// user-visible text identical to the raw model output when a
				// link never materializes.
				contentBuilder.WriteRune(e.text[openIndex])
				e.flushedRunes = openIndex + 1
				continue
			}
			// Wait for more input before committing this tail.
			return contentBuilder.String(), annotations
		case linkInvalid:
			// The span at openIndex is definitely not a link (e.g. `[...]`
			// without a following `(`). Flush only the opening bracket so
			// subsequent scanning re-examines the rest of the span in case
			// a real link starts inside it.
			contentBuilder.WriteRune(e.text[openIndex])
			e.flushedRunes = openIndex + 1
		}
	}

	return contentBuilder.String(), annotations
}

// findNextLinkOpen returns the index of the next opening bracket that could
// start a markdown link at or after `from`. Both ASCII `[` and fullwidth
// `\u3010` are tracked so the emitter can rewrite the near-markdown shape
// some web-search-tuned models emit before handing text to clients.
func findNextLinkOpen(text []rune, from int) (int, bool) {
	for i := from; i < len(text); i++ {
		r := text[i]
		if r == '[' || r == '\u3010' {
			return i, true
		}
	}
	return 0, false
}

type linkMatchOutcome int

const (
	linkMatch linkMatchOutcome = iota
	linkIncomplete
	linkInvalid
)

// matchMarkdownLink examines the rune slice starting at start (which must
// point at `[` or `\u3010`) and reports whether it contains a complete
// markdown link of the form `[label](url)` (or the fullwidth equivalent).
//
// When the outcome is linkMatch, end is the exclusive rune index one past
// the closing `)` and labelStart/labelEnd span the label runes (between the
// brackets) in rune space, suitable for use as `start_index` / `end_index`
// on url_citation annotations. url is the matched URL string.
//
// When the outcome is linkIncomplete, the caller should wait for more input
// because the span at `start` could still grow into a match. When the
// outcome is linkInvalid, the caller should treat the opening bracket as
// plain content and resume scanning from the next rune.
func matchMarkdownLink(text []rune, start int) (outcome linkMatchOutcome, end, labelStart, labelEnd int, url string) {
	if start >= len(text) {
		return linkIncomplete, 0, 0, 0, ""
	}
	open := text[start]
	switch open {
	case '[', '\u3010':
	default:
		return linkInvalid, 0, 0, 0, ""
	}

	// Accept either ASCII ']' or fullwidth '\u3011' as the closer regardless
	// of which opener the model produced. gpt-oss observed in the wild emits
	// mixed-bracket spans such as `【label](url)` when improvising a citation
	// format outside the harmony browser tool training distribution.
	labelStart = start + 1
	labelEnd = -1
	for i := labelStart; i < len(text); i++ {
		if text[i] == ']' || text[i] == '\u3011' {
			labelEnd = i
			break
		}
	}
	if labelEnd < 0 {
		return linkIncomplete, 0, 0, 0, ""
	}
	if labelEnd == labelStart {
		// Empty label: `[](...)` is not something the model is ever asked to
		// produce; treat it as invalid so the opening bracket is flushed.
		return linkInvalid, 0, 0, 0, ""
	}

	parenIdx := labelEnd + 1
	if parenIdx >= len(text) {
		return linkIncomplete, 0, 0, 0, ""
	}
	if text[parenIdx] != '(' {
		return linkInvalid, 0, 0, 0, ""
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
			// URLs in our matcher disallow whitespace, mirroring the regex
			// used by the non-streaming citation post-processor.
			return linkInvalid, 0, 0, 0, ""
		}
	}
	if urlEnd < 0 {
		return linkIncomplete, 0, 0, 0, ""
	}

	url = string(text[urlStart:urlEnd])
	if !isSupportedURL(url) {
		return linkInvalid, 0, 0, 0, ""
	}

	return linkMatch, urlEnd + 1, labelStart, labelEnd, url
}

// normalizeLinkBrackets rewrites fullwidth brackets to ASCII so the text the
// emitter forwards downstream always renders as a standard markdown link,
// regardless of which shape the model produced. Called only after
// matchMarkdownLink reports linkMatch so the bracket positions are known.
func normalizeLinkBrackets(text []rune, start, end int) {
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

// isSpaceRune matches the subset of whitespace the markdown URL matcher
// treats as a delimiter. Keeping this in sync with the regex used by the
// non-streaming post-processor avoids two divergent definitions of "valid
// URL character" inside the same package.
func isSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// isSupportedURL reports whether a URL string has one of the schemes the
// non-streaming post-processor accepts as a citation target. Anything else
// is treated as plain text so a mis-shaped link does not produce a broken
// annotation on the client.
func isSupportedURL(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

// resolveSource returns the most informative recorded source for the given
// URL, or (zero, false) when the URL was never registered during the tool
// loop. Mirrors the behavior of citationState.matchesFor so streaming and
// non-streaming annotations agree on title-resolution tiebreaks.
func (c *citationState) resolveSource(url string) (citationSource, bool) {
	if c == nil || url == "" {
		return citationSource{}, false
	}
	var best citationSource
	found := false
	for _, source := range c.sources {
		if source.url != url {
			continue
		}
		if !found || (best.title == "" && source.title != "") {
			best = source
			found = true
		}
	}
	return best, found
}
