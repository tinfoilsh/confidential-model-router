//go:build toolruntime_debug

package toolruntime

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// debugEnabled is a compile-time true constant when the toolruntime_debug
// build tag is set. In production builds (no build tag) this symbol is
// replaced by a compile-time false constant in debug_disabled.go so every
// call site is dead-code-eliminated.
const debugEnabled = true

var debugTraceCounter uint64

func debugTraceID() string {
	return fmt.Sprintf("t%04d", atomic.AddUint64(&debugTraceCounter, 1))
}

func debugLogf(format string, args ...any) {
	log.Printf(format, args...)
}

func debugPreview(v any, max int) string {
	var s string
	switch value := v.(type) {
	case string:
		s = value
	case []byte:
		s = string(value)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			s = fmt.Sprintf("%+v", v)
		} else {
			s = string(b)
		}
	}
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("...(+%d bytes)", len(s)-max)
}

func debugMessagesSummary(messages []any, contentMax int) string {
	parts := make([]string, 0, len(messages))
	for i, raw := range messages {
		m, _ := raw.(map[string]any)
		role := stringValue(m["role"])
		name := stringValue(m["name"])
		content := m["content"]
		toolCalls, _ := m["tool_calls"].([]any)
		var summary string
		if len(toolCalls) > 0 {
			names := make([]string, 0, len(toolCalls))
			for _, tc := range toolCalls {
				tm, _ := tc.(map[string]any)
				fn, _ := tm["function"].(map[string]any)
				names = append(names, stringValue(fn["name"]))
			}
			summary = fmt.Sprintf("tool_calls=%v", names)
		} else if content != nil {
			summary = debugPreview(content, contentMax)
		}
		if name != "" {
			parts = append(parts, fmt.Sprintf("[%d]%s(%s): %s", i, role, name, summary))
		} else {
			parts = append(parts, fmt.Sprintf("[%d]%s: %s", i, role, summary))
		}
	}
	return strings.Join(parts, " | ")
}

// ---------------------------------------------------------------------------
// devLog — human-readable session log written to logs/<session-id>.txt
// ---------------------------------------------------------------------------

// devLog writes a human-readable session log to logs/<session-id>.txt
// during local development (DEBUG=1 + toolruntime_debug build tag). All
// methods are nil-safe: a nil *devLog is a no-op, so call sites are
// unconditional.
//
// Production builds get the no-op stub in debug_disabled.go, which the
// compiler eliminates entirely, so user content never touches disk in a
// deployed enclave regardless of the runtime DEBUG flag.
type devLog struct {
	mu   sync.Mutex
	file *os.File
}

// openDevLog derives the session ID from the request, creates the logs/
// directory (if needed), opens logs/<session-id>.txt in append mode, and
// writes the initial header block. The session ID is sanitized to its
// base name so a client-controlled X-Session-Id header cannot escape the
// logs/ directory. Returns nil on any error so callers never have to
// nil-check beyond the initial open.
func openDevLog(r *http.Request, body map[string]any, modelName string, registry *sessionRegistry) *devLog {
	sid := r.Header.Get("X-Session-Id")
	if sid == "" {
		sid = "no-session-" + debugTraceID()
	}
	// Defense-in-depth: strip directory components so a malicious
	// X-Session-Id value like "../../etc/cron.d/evil" cannot write
	// outside the logs/ directory.
	sid = filepath.Base(sid)

	dir := "logs"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	path := filepath.Join(dir, sid+".txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	d := &devLog{file: f}
	d.writef("\n%s\n", devLogSeparator)
	d.writef("User: %s\n", truncate(extractLastUserMessage(body), 200))
	d.writef("Model: %s\n", modelName)
	d.writef("Session: %s\n", sid)
	if endpoints := strings.Join(registry.endpointSummary(), ", "); endpoints != "" {
		d.writef("MCP endpoint: %s\n", endpoints)
	}
	d.writef("%s\n", devLogSeparator)
	return d
}

// Close syncs and closes the underlying file.
func (d *devLog) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.file.Sync()
	_ = d.file.Close()
}

func (d *devLog) writef(format string, args ...any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.file, format, args...)
}

const devLogSeparator = "============================================================"

// WriteTurn logs the turn separator, token usage, and thinking/content
// extracted from a non-streaming upstream response body.
func (d *devLog) WriteTurn(turn int, responseBody map[string]any) {
	if d == nil {
		return
	}
	d.writef("\n%s\n", devLogSeparator)
	d.writef("Turn %d\n", turn)
	d.writef("%s\n", devLogSeparator)
	if usage, ok := responseBody["usage"].(map[string]any); ok {
		d.writeTokens(usage)
	}
	thinking, content := extractThinkingAndContent(responseBody)
	if thinking != "" {
		d.writef("[thinking] %s\n", truncate(strings.TrimSpace(thinking), 500))
	}
	if content != "" {
		d.writef("[content] %s\n", truncate(strings.TrimSpace(content), 500))
	}
}

// WriteStreamedTurn logs the turn separator, token usage, and
// pre-extracted thinking/content from a streaming iteration. The
// streaming path accumulates these values incrementally rather than
// extracting them from a final response body.
func (d *devLog) WriteStreamedTurn(turn int, usage map[string]any, thinking, content string) {
	if d == nil {
		return
	}
	d.writef("\n%s\n", devLogSeparator)
	d.writef("Turn %d\n", turn)
	d.writef("%s\n", devLogSeparator)
	d.writeTokens(usage)
	if thinking != "" {
		d.writef("[thinking] %s\n", truncate(strings.TrimSpace(thinking), 500))
	}
	if content != "" {
		d.writef("[content] %s\n", truncate(strings.TrimSpace(content), 500))
	}
}

func (d *devLog) writeTokens(usage map[string]any) {
	if usage == nil {
		return
	}
	// Chat uses prompt_tokens/completion_tokens; Responses uses input_tokens/output_tokens.
	prompt := intFromAny(usage["prompt_tokens"])
	if prompt == 0 {
		prompt = intFromAny(usage["input_tokens"])
	}
	completion := intFromAny(usage["completion_tokens"])
	if completion == 0 {
		completion = intFromAny(usage["output_tokens"])
	}
	total := intFromAny(usage["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	d.writef("[tokens] prompt=%d completion=%d total=%d\n", prompt, completion, total)
}

// WriteToolCalls writes the tool calls summary.
func (d *devLog) WriteToolCalls(calls []toolCall) {
	if d == nil || len(calls) == 0 {
		return
	}
	d.writef("[tool_calls] %d call(s)\n", len(calls))
	for i, c := range calls {
		argsJSON, _ := json.Marshal(c.arguments)
		d.writef("  [%d] %s(%s) id=%s\n", i, c.name, truncate(string(argsJSON), 200), c.id)
	}
}

// WriteToolExec writes a single tool execution result.
func (d *devLog) WriteToolExec(name string, args map[string]any, output string, elapsed time.Duration, errMsg string) {
	if d == nil {
		return
	}
	argsJSON, _ := json.Marshal(args)
	d.writef("\n[exec] %s(%s) elapsed=%s\n", name, truncate(string(argsJSON), 200), elapsed.Round(time.Millisecond))
	if errMsg != "" {
		d.writef("[error] %s\n", errMsg)
	}
	d.writef("[result]\n%s\n", truncate(output, 2000))
}

// WriteFinish writes the finish reason line.
func (d *devLog) WriteFinish(reason string) {
	if d == nil || reason == "" {
		return
	}
	d.writef("[finish] %s\n", reason)
}

// ---------------------------------------------------------------------------
// Extraction helpers — only needed by devLog, so they live behind the
// build tag and are absent from production binaries.
// ---------------------------------------------------------------------------

// extractLastUserMessage pulls the last user message from the request body.
// Works for both Chat Completions (messages array) and Responses (input array).
func extractLastUserMessage(body map[string]any) string {
	// Chat Completions: body.messages
	if messages, ok := body["messages"].([]any); ok {
		for i := len(messages) - 1; i >= 0; i-- {
			msg, _ := messages[i].(map[string]any)
			if msg == nil {
				continue
			}
			if stringValue(msg["role"]) == "user" {
				return extractContentString(msg["content"])
			}
		}
	}
	// Responses: body.input (can be a string or array)
	if input, ok := body["input"].(string); ok {
		return input
	}
	if input, ok := body["input"].([]any); ok {
		for i := len(input) - 1; i >= 0; i-- {
			item, _ := input[i].(map[string]any)
			if item == nil {
				continue
			}
			if stringValue(item["role"]) == "user" || stringValue(item["type"]) == "message" {
				return extractContentString(item["content"])
			}
		}
	}
	return ""
}

// extractContentString handles the various shapes content can take:
// a plain string, or an array of content parts.
func extractContentString(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	if parts, ok := content.([]any); ok {
		var sb strings.Builder
		for _, part := range parts {
			if p, ok := part.(map[string]any); ok {
				if text := stringValue(p["text"]); text != "" {
					if sb.Len() > 0 {
						sb.WriteString(" ")
					}
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// extractThinkingAndContent extracts the reasoning and content text from
// an upstream response body. Works for Chat Completions
// (choices[0].message.reasoning_content / content) and Responses
// (output[].type=="reasoning" / output[].type=="message").
func extractThinkingAndContent(responseBody map[string]any) (thinking, content string) {
	// Chat Completions: choices[0].message
	if choices, ok := responseBody["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if msg, ok := ch["message"].(map[string]any); ok {
				thinking = firstNonEmptyString(msg["reasoning_content"], msg["reasoning"])
				content = stringValue(msg["content"])
				return
			}
		}
	}
	// Responses: iterate output items
	if output, ok := responseBody["output"].([]any); ok {
		for _, item := range output {
			o, _ := item.(map[string]any)
			if o == nil {
				continue
			}
			switch stringValue(o["type"]) {
			case "reasoning":
				if summary, ok := o["summary"].([]any); ok {
					for _, s := range summary {
						if sm, ok := s.(map[string]any); ok {
							thinking += stringValue(sm["text"])
						}
					}
				}
			case "message":
				if contentArr, ok := o["content"].([]any); ok {
					for _, c := range contentArr {
						if cm, ok := c.(map[string]any); ok {
							content += stringValue(cm["text"])
						}
					}
				}
			}
		}
	}
	return
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
