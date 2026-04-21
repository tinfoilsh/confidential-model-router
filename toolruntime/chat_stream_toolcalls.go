package toolruntime

import "encoding/json"

// chatToolCallBuilder assembles OpenAI streaming tool_call deltas into the
// final tool_call objects. OpenAI emits each function's name and id on
// the first delta for a given index, and the arguments JSON in incremental
// string fragments; we replay that protocol byte-for-byte.
type chatToolCallBuilder struct {
	entries []*chatToolCallEntry
}

type chatToolCallEntry struct {
	index        int
	id           string
	toolType     string
	functionName string
	arguments    []byte
}

// ingest merges one tool_call delta list into the builder's running state.
// It preserves the original wire-format fragment in s.raw so the assistant
// message emitted on the next iteration carries bytewise-identical tool
// calls to whatever upstream produced.
func (b *chatToolCallBuilder) ingest(rawCalls []any) {
	for _, item := range rawCalls {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		idx := int(numberValue(m["index"]))
		if idx < 0 {
			// OpenAI's streaming tool_call protocol uses a 0-based
			// index; a negative value is either a protocol bug or
			// a hostile/malformed upstream frame. Dropping the
			// delta is safer than indexing into b.entries with a
			// negative int, which would panic the streamer.
			continue
		}
		for len(b.entries) <= idx {
			b.entries = append(b.entries, &chatToolCallEntry{index: len(b.entries)})
		}
		entry := b.entries[idx]
		if id := stringValue(m["id"]); id != "" {
			entry.id = id
		}
		if t := stringValue(m["type"]); t != "" {
			entry.toolType = t
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if name := stringValue(fn["name"]); name != "" {
				entry.functionName = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				entry.arguments = append(entry.arguments, args...)
			}
		}
	}
}

// toolCalls returns the assembled tool_call objects as the parsed router
// type. Arguments are JSON-decoded when possible; malformed arguments are
// preserved as an empty map so the loop can still surface the tool call
// rather than silently drop it.
func (b *chatToolCallBuilder) toolCalls() []toolCall {
	out := make([]toolCall, 0, len(b.entries))
	for _, entry := range b.entries {
		if entry == nil || entry.functionName == "" {
			continue
		}
		args := map[string]any{}
		if len(entry.arguments) > 0 {
			raw, _ := sanitizeToolCallArgumentsJSON(entry.arguments)
			_ = json.Unmarshal(raw, &args)
		}
		out = append(out, toolCall{
			id:        entry.id,
			name:      entry.functionName,
			arguments: args,
		})
	}
	return out
}

// raw returns the tool_calls array in the wire shape OpenAI uses for
// non-streaming completions. Used to rehydrate the assistant message we
// prepend to the next iteration's messages array.
func (b *chatToolCallBuilder) raw() []any {
	calls := make([]any, 0, len(b.entries))
	for _, entry := range b.entries {
		if entry == nil || entry.functionName == "" {
			continue
		}
		toolType := entry.toolType
		if toolType == "" {
			toolType = "function"
		}
		calls = append(calls, map[string]any{
			"id":   entry.id,
			"type": toolType,
			"function": map[string]any{
				"name":      entry.functionName,
				"arguments": string(entry.arguments),
			},
		})
	}
	return calls
}

// rawToolCallsFromParsed filters the streamed raw tool_calls down to the
// client-owned ones and re-indexes them starting at zero so the final
// stream chunk emits a delta.tool_calls array in the shape clients expect:
// contiguous `index` values identifying positions in the client-visible
// tool_calls list. The `id`, `type`, and `function` fields are preserved
// byte-for-byte from what upstream produced.
func rawToolCallsFromParsed(parsed []toolCall, raw []any) []any {
	allowed := make(map[string]struct{}, len(parsed))
	for _, call := range parsed {
		allowed[call.id] = struct{}{}
	}
	filtered := make([]any, 0, len(parsed))
	for _, item := range raw {
		source, _ := item.(map[string]any)
		if _, ok := allowed[stringValue(source["id"])]; !ok {
			continue
		}
		reindexed := map[string]any{
			"index":    len(filtered),
			"id":       source["id"],
			"type":     source["type"],
			"function": source["function"],
		}
		filtered = append(filtered, reindexed)
	}
	return filtered
}
