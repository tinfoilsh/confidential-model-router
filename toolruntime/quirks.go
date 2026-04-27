// Package-private router-side defenses against known upstream bugs in the
// tool-calling streaming decoders of specific models and inference stacks.
// Repairs are isolated in this file so the whole set can be removed once
// upstream fixes (vLLM Enforcer, Kimi chat templates, Gemma URL wrapping)
// ship broadly.

package toolruntime

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// quirksEnabled is the single kill switch. Flip to false to observe raw
// upstream behavior for a repro session without ripping out the call sites.
const quirksEnabled = true

// controlTokenPattern matches the model-control token shape that Kimi,
// Gemma, and several other recent models use for tool-call framing. The
// shape varies by model: Kimi emits <|tool_call_argument_begin|> (opens
// with `<|`, closes with `|>`), while Gemma's streaming decoder emits
// <|"| (opens with `<|`, closes with `|`) when wrapping URL fragments.
// We therefore match either close form: `|>` OR a bare `|` not followed
// by `>`. The inner class forbids `|`, `<`, and `>` so we cannot span
// across two adjacent tokens, and is capped at 64 runes so a pathological
// input cannot DoS the stripper. Compiled once at package init because
// the call path runs on every tool call.
var controlTokenPattern = regexp.MustCompile(`<\|[^|<>]{0,64}\|>?`)

// urlFieldAllowlist enumerates the argument keys whose string values are
// unambiguously URLs and therefore safe to strip model control tokens from
// even when the surrounding context would otherwise be suspect. Any other
// string is left alone unless its value has the http(s):// prefix, to keep
// the stripper conservative (see deepStripURLTokens).
var urlFieldAllowlist = map[string]struct{}{
	"url":       {},
	"urls":      {},
	"fetch_url": {},
	"page_url":  {},
	"href":      {},
	"link":      {},
}

// sanitizeToolCallArguments applies the router-side quirk repairs to a
// single tool_call's parsed arguments map in place, and reports whether
// anything actually changed. Safe to call on a clean input -- returns
// false and does not touch the map.
func sanitizeToolCallArguments(toolName string, args map[string]any) bool {
	if !quirksEnabled || len(args) == 0 {
		return false
	}
	changed := deepStripURLTokens(args)
	if changed {
		debugLogf("toolruntime:quirk strip_tokens tool=%s", toolName)
	}
	return changed
}

// stripModelControlTokens removes every <|...|> model-control token from
// s. Idempotent: running it twice on the same string yields the same
// result as running it once. Returns s unchanged (same underlying string)
// when no token is present, so the common path allocates nothing.
func stripModelControlTokens(s string) string {
	if !strings.Contains(s, "<|") {
		return s
	}
	return controlTokenPattern.ReplaceAllString(s, "")
}

// looksLikeLeakedURL reports whether a string is almost certainly a URL
// with model control tokens leaked around it. Used to detect leaks in
// fields that are NOT in urlFieldAllowlist, so we only strip when we are
// confident the value is really a URL rather than arbitrary user text.
func looksLikeLeakedURL(s string) bool {
	if !strings.Contains(s, "<|") {
		return false
	}
	stripped := stripModelControlTokens(s)
	return strings.HasPrefix(stripped, "http://") || strings.HasPrefix(stripped, "https://")
}

// deepStripURLTokens walks a parsed JSON value (as produced by
// json.Unmarshal into an any) and rewrites every string that is either
// (a) at a key in urlFieldAllowlist, or (b) looks like a leaked URL, to
// remove embedded <|...|> control tokens. Reports whether any string was
// actually changed. Recurses into maps and slices. Never allocates if the
// input is clean.
func deepStripURLTokens(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		changed := false
		for key, child := range val {
			_, isURLField := urlFieldAllowlist[strings.ToLower(key)]
			switch c := child.(type) {
			case string:
				if isURLField {
					if stripped := stripModelControlTokens(c); stripped != c {
						val[key] = stripped
						changed = true
					}
				} else if looksLikeLeakedURL(c) {
					val[key] = stripModelControlTokens(c)
					changed = true
				}
			case []any:
				for i, item := range c {
					if s, ok := item.(string); ok {
						if isURLField {
							if stripped := stripModelControlTokens(s); stripped != s {
								c[i] = stripped
								changed = true
							}
						} else if looksLikeLeakedURL(s) {
							c[i] = stripModelControlTokens(s)
							changed = true
						}
					} else if deepStripURLTokens(item) {
						changed = true
					}
				}
			case map[string]any:
				if deepStripURLTokens(c) {
					changed = true
				}
			}
		}
		return changed
	case []any:
		changed := false
		for _, item := range val {
			if deepStripURLTokens(item) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// sanitizeToolCallArgumentsJSON repairs a raw argument byte string before
// it is handed to json.Unmarshal. This is the streaming-only entry point:
// the incremental builder accumulates argument deltas as raw bytes, and
// some models (notably Kimi-K2 "thinking") occasionally append reasoning
// text immediately after the closing `}` of the JSON object, producing a
// payload like `{"a":1}Now I will...` that json.Unmarshal rejects.
//
// The repair strategy is intentionally conservative: try json.Valid first
// and return the input untouched if it is already a well-formed value, so
// clean payloads allocate nothing. Otherwise, extract the first complete
// top-level JSON object using a brace-balanced scanner that respects
// double-quoted strings and `\` escapes; if that succeeds, return the
// extracted object. Failing both, return the input unchanged and let the
// caller surface the parse error normally.
func sanitizeToolCallArgumentsJSON(raw []byte) ([]byte, bool) {
	if !quirksEnabled || len(raw) == 0 {
		return raw, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, false
	}
	// json.Valid is the cheap path: if the bytes already parse as JSON,
	// no repair is needed regardless of what shape the surrounding code
	// wants.
	if jsonBytesValid(trimmed) {
		return raw, false
	}
	// Only attempt object-extraction when the payload clearly starts
	// with an object. Arrays, primitives, and garbage are left for the
	// caller to reject.
	if trimmed[0] != '{' {
		return raw, false
	}
	obj, ok := extractFirstJSONObject(string(trimmed))
	if !ok {
		return raw, false
	}
	repaired := []byte(obj)
	debugLogf("toolruntime:quirk json_repair before_len=%d after_len=%d", len(raw), len(repaired))
	return repaired, true
}

// extractFirstJSONObject scans s for the first syntactically-balanced
// top-level JSON object `{...}` and returns it as a new string, along
// with true on success. Respects double-quoted strings (so a `}` inside a
// string literal does not close the object prematurely) and `\` escapes
// (so `\"` inside a string literal does not end the string).
//
// Returns ("", false) when s does not start with `{`, when the scanner
// reaches the end of the input without closing the top-level object, or
// when the input is empty.
func extractFirstJSONObject(s string) (string, bool) {
	if len(s) == 0 || s[0] != '{' {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			// The previous byte was an unescaped `\` inside a string;
			// whatever this byte is, it is a literal and cannot end the
			// string or affect depth.
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
			if depth < 0 {
				// Stray closing brace outside any object; bail rather
				// than continue scanning into what is almost certainly
				// garbage.
				return "", false
			}
		}
	}
	return "", false
}

// sanitizeAssistantToolCallsInMessages walks a chat-completions messages
// slice and rewrites any assistant-message tool_call.function.arguments
// that still fails to parse as JSON to "{}". Returns the number of
// arguments strings that were replaced. Used to prevent one malformed
// streamed frame from poisoning session history and causing every
// subsequent upstream request to 400 forever (see MoonshotAI/kimi-cli
// #1171 for the canonical description of this failure mode).
//
// The function is a no-op on messages that are already clean, so it is
// safe to call once per tool-loop iteration without amplifying CPU cost.
// Only messages with role == "assistant" are inspected; user, system,
// and tool messages are left alone.
func sanitizeAssistantToolCallsInMessages(messages []any) int {
	if !quirksEnabled {
		return 0
	}
	changed := 0
	for idx, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "assistant" {
			continue
		}
		toolCalls, ok := m["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, rawCall := range toolCalls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			args, ok := fn["arguments"].(string)
			if !ok || args == "" {
				continue
			}
			if jsonBytesValid([]byte(args)) {
				continue
			}
			// Try our repair before giving up: this recovers the
			// JSON+trailing-prose shape we already handle elsewhere.
			if repaired, repairedOK := sanitizeToolCallArgumentsJSON([]byte(args)); repairedOK && jsonBytesValid(repaired) {
				fn["arguments"] = string(repaired)
				changed++
				debugLogf("toolruntime:quirk history_sanitize msg_idx=%d repaired=true", idx)
				continue
			}
			fn["arguments"] = "{}"
			changed++
			debugLogf("toolruntime:quirk history_sanitize msg_idx=%d replaced_with_empty=true", idx)
		}
	}
	return changed
}

// jsonBytesValid reports whether b is a syntactically valid JSON value.
// Thin wrapper over encoding/json.Valid kept here so a future refactor
// that swaps the underlying check (e.g. for a faster custom scanner) has
// exactly one call site to update.
func jsonBytesValid(b []byte) bool {
	return json.Valid(b)
}
