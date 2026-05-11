package manager

import (
	"encoding/json"
	"io"
	"strconv"
	"sync"

	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

const (
	maxJSONKeyBytes   = 256
	maxUsageJSONBytes = 64 << 10
)

type streamingJSONUsageReadCloser struct {
	reader       io.Reader
	body         io.ReadCloser
	extractor    *topLevelUsageExtractor
	usageHandler func(*tokencount.Usage)
	once         sync.Once
	closeErr     error
}

func newStreamingJSONUsageReadCloser(body io.ReadCloser, usageHandler func(*tokencount.Usage)) io.ReadCloser {
	extractor := &topLevelUsageExtractor{}
	return &streamingJSONUsageReadCloser{
		reader:       io.TeeReader(body, extractor),
		body:         body,
		extractor:    extractor,
		usageHandler: usageHandler,
	}
}

func (r *streamingJSONUsageReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *streamingJSONUsageReadCloser) Close() error {
	r.once.Do(func() {
		r.closeErr = r.body.Close()
		if usage := r.extractor.Usage(); usage != nil && r.usageHandler != nil {
			r.usageHandler(usage)
		}
	})
	return r.closeErr
}

type topLevelUsageExtractor struct {
	depth int

	inString   bool
	escaped    bool
	readingKey bool

	expectingKey       bool
	awaitingUsageColon bool
	awaitingUsageValue bool

	keyBuf      []byte
	keyTooLarge bool

	capturingUsage bool
	usageDepth     int
	usageInString  bool
	usageEscaped   bool
	usageBuf       []byte
	usageTooLarge  bool

	done  bool
	usage *tokencount.Usage
}

func (e *topLevelUsageExtractor) Write(p []byte) (int, error) {
	for _, c := range p {
		e.consume(c)
	}
	return len(p), nil
}

func (e *topLevelUsageExtractor) Usage() *tokencount.Usage {
	return e.usage
}

func (e *topLevelUsageExtractor) consume(c byte) {
	if e.done {
		return
	}
	if e.capturingUsage {
		e.consumeUsageByte(c)
		return
	}
	if e.inString {
		e.consumeStringByte(c)
		return
	}
	if e.awaitingUsageValue {
		if isJSONSpace(c) {
			return
		}
		if c == '{' {
			e.startUsageCapture(c)
			return
		}
		e.done = true
		return
	}
	if e.awaitingUsageColon {
		if isJSONSpace(c) {
			return
		}
		if c == ':' {
			e.awaitingUsageColon = false
			e.awaitingUsageValue = true
			return
		}
		e.done = true
		return
	}

	switch c {
	case '"':
		e.inString = true
		e.escaped = false
		e.readingKey = e.depth == 1 && e.expectingKey
		if e.readingKey {
			e.keyBuf = e.keyBuf[:0]
			e.keyTooLarge = false
		}
	case '{':
		e.depth++
		if e.depth == 1 {
			e.expectingKey = true
		}
	case '[':
		e.depth++
	case '}':
		if e.depth == 1 {
			e.expectingKey = false
		}
		if e.depth > 0 {
			e.depth--
		}
	case ']':
		if e.depth > 0 {
			e.depth--
		}
	case ':':
		if e.depth == 1 {
			e.expectingKey = false
		}
	case ',':
		if e.depth == 1 {
			e.expectingKey = true
		}
	}
}

func (e *topLevelUsageExtractor) consumeStringByte(c byte) {
	if e.escaped {
		e.escaped = false
		e.captureKeyByte(c)
		return
	}
	if c == '\\' {
		e.escaped = true
		e.captureKeyByte(c)
		return
	}
	if c == '"' {
		e.inString = false
		if e.readingKey {
			e.finishKey()
			e.readingKey = false
		}
		return
	}
	e.captureKeyByte(c)
}

func (e *topLevelUsageExtractor) captureKeyByte(c byte) {
	if !e.readingKey || e.keyTooLarge {
		return
	}
	if len(e.keyBuf) >= maxJSONKeyBytes {
		e.keyTooLarge = true
		e.keyBuf = e.keyBuf[:0]
		return
	}
	e.keyBuf = append(e.keyBuf, c)
}

func (e *topLevelUsageExtractor) finishKey() {
	if e.keyTooLarge {
		return
	}
	key, err := strconv.Unquote(`"` + string(e.keyBuf) + `"`)
	if err == nil && key == "usage" {
		e.awaitingUsageColon = true
		e.expectingKey = false
	}
}

func (e *topLevelUsageExtractor) startUsageCapture(c byte) {
	e.awaitingUsageValue = false
	e.capturingUsage = true
	e.usageDepth = 1
	e.usageInString = false
	e.usageEscaped = false
	e.captureUsageByte(c)
}

func (e *topLevelUsageExtractor) consumeUsageByte(c byte) {
	e.captureUsageByte(c)
	if e.usageInString {
		if e.usageEscaped {
			e.usageEscaped = false
			return
		}
		if c == '\\' {
			e.usageEscaped = true
			return
		}
		if c == '"' {
			e.usageInString = false
		}
		return
	}

	switch c {
	case '"':
		e.usageInString = true
	case '{', '[':
		e.usageDepth++
	case '}', ']':
		e.usageDepth--
		if e.usageDepth == 0 {
			e.finishUsage()
		}
	}
}

func (e *topLevelUsageExtractor) captureUsageByte(c byte) {
	if e.usageTooLarge {
		return
	}
	if len(e.usageBuf) >= maxUsageJSONBytes {
		e.usageTooLarge = true
		e.usageBuf = e.usageBuf[:0]
		return
	}
	e.usageBuf = append(e.usageBuf, c)
}

func (e *topLevelUsageExtractor) finishUsage() {
	e.done = true
	e.capturingUsage = false
	if e.usageTooLarge {
		return
	}
	var usage tokencount.Usage
	if err := json.Unmarshal(e.usageBuf, &usage); err == nil {
		usage.Normalize()
		e.usage = &usage
	}
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}
