package toolruntime

import (
	"bufio"
	"io"
	"strings"
)

// sseFrame is a single parsed Server-Sent Events frame.
//
// OpenAI's Chat Completions streaming wire format emits only `data: ...` lines
// terminated by a blank line, so the event name stays empty. The Responses
// streaming wire format uses `event: <name>` lines before each `data:` block,
// which we need to preserve so downstream consumers can dispatch on type.
type sseFrame struct {
	// event is the SSE event name from an `event:` line. Empty when the
	// upstream only emits `data:` frames.
	event string
	// data holds the raw payload from one or more `data:` lines joined by
	// newlines, as defined by the SSE spec. Callers interpret this as JSON
	// (typical for OpenAI) or the sentinel string "[DONE]".
	data string
}

// sseReader is a small incremental parser for Server-Sent Events that groups
// lines into `sseEvent` values separated by blank lines, as specified by the
// HTML5 SSE grammar.
//
// It is intentionally minimal: we only need the subset of SSE OpenAI emits,
// which is `event:` and `data:` lines. Comments (`: ...`) and `id:` / `retry:`
// fields are ignored.
type sseReader struct {
	scanner *bufio.Scanner
}

// newSSEReader wraps an io.Reader with an SSE frame splitter. The underlying
// bufio.Scanner gets a large max buffer so single SSE frames (which can exceed
// 64KiB when a model emits a large tool-call argument blob in one chunk) do
// not trigger bufio.ErrTooLong.
func newSSEReader(r io.Reader) *sseReader {
	scanner := bufio.NewScanner(r)
	const maxSSELineBytes = 1 << 22 // 4 MiB
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	return &sseReader{scanner: scanner}
}

// next returns the next parsed SSE event or io.EOF when the stream ends.
// Partial trailing events (no terminating blank line) are returned on EOF
// rather than silently dropped so callers do not miss the last chunk of a
// connection that was closed without a final newline.
//
// Per the HTML5 SSE dispatch rules an event is only emitted when it
// carries at least one `data` field; comment-only or heartbeat-only
// frames (`: ping\n\n`) are swallowed entirely rather than surfaced to
// the caller as empty-data events.
func (r *sseReader) next() (sseFrame, error) {
	var (
		event     sseFrame
		dataLines []string
		haveData  bool
	)
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if haveData {
				event.data = strings.Join(dataLines, "\n")
				return event, nil
			}
			// Reset any accumulated non-data fields (for example a
			// stray `event:` with no following `data:`) so they do
			// not leak into the next real frame.
			event = sseFrame{}
			dataLines = dataLines[:0]
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value := splitSSEField(line)
		switch name {
		case "event":
			event.event = value
		case "data":
			dataLines = append(dataLines, value)
			haveData = true
		}
	}
	if err := r.scanner.Err(); err != nil {
		return sseFrame{}, err
	}
	if haveData {
		event.data = strings.Join(dataLines, "\n")
		return event, nil
	}
	return sseFrame{}, io.EOF
}

// splitSSEField splits an SSE field line into its name and value per the spec:
// the first colon separates them and a single leading space in the value is
// stripped. Lines without a colon are treated as field-name only with an
// empty value, matching the HTML5 SSE grammar.
func splitSSEField(line string) (string, string) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return line, ""
	}
	name := line[:idx]
	value := line[idx+1:]
	value = strings.TrimPrefix(value, " ")
	return name, value
}
