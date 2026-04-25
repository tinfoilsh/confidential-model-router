package toolruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// devLog writes a human-readable session log to logs/<session-id>.txt
// during local development (DEBUG=1). All methods are nil-safe: a nil
// *devLog is a no-op, so call sites are unconditional.
type devLog struct {
	mu   sync.Mutex
	file *os.File
}

// openDevLog creates the logs/ directory (if needed) and opens
// logs/<sessionID>.txt in append mode. Returns nil on any error so
// callers never have to nil-check beyond the initial open.
func openDevLog(sessionID string) *devLog {
	dir := "logs"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	path := filepath.Join(dir, sessionID+".txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return &devLog{file: f}
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

// WriteHeader writes the session header block at the top of a new request.
func (d *devLog) WriteHeader(userMessage, modelName, sessionID, mcpEndpoints string) {
	if d == nil {
		return
	}
	d.writef("\n%s\n", devLogSeparator)
	d.writef("User: %s\n", truncate(userMessage, 200))
	d.writef("Model: %s\n", modelName)
	d.writef("Session: %s\n", sessionID)
	if mcpEndpoints != "" {
		d.writef("MCP endpoint: %s\n", mcpEndpoints)
	}
	d.writef("%s\n", devLogSeparator)
}

// WriteTurnHeader writes the turn separator.
func (d *devLog) WriteTurnHeader(turn int) {
	if d == nil {
		return
	}
	d.writef("\n%s\n", devLogSeparator)
	d.writef("Turn %d\n", turn)
	d.writef("%s\n", devLogSeparator)
}

// WriteTokens writes the token usage line.
func (d *devLog) WriteTokens(usage map[string]any) {
	if d == nil || usage == nil {
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

// WriteThinking writes the thinking/reasoning line.
func (d *devLog) WriteThinking(reasoning string) {
	if d == nil || reasoning == "" {
		return
	}
	d.writef("[thinking] %s\n", truncate(strings.TrimSpace(reasoning), 500))
}

// WriteContent writes the model content line.
func (d *devLog) WriteContent(content string) {
	if d == nil || content == "" {
		return
	}
	d.writef("[content] %s\n", truncate(strings.TrimSpace(content), 500))
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
