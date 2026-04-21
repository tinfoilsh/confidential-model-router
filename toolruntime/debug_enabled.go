//go:build toolruntime_debug

package toolruntime

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
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
