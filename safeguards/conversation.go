// Package safeguards submits completed first-party chat conversations to the
// safeguards sidecar for acceptable-use-policy classification.
package safeguards

import (
	"fmt"
	"strings"
)

// Message is one turn in the sidecar's ingest format. Content is always plain
// text; non-text parts are replaced with a placeholder so binary data never
// leaves the router.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatMessages flattens a chat-completions messages array. Entries the
// sidecar cannot classify (tool results, malformed items) are skipped.
func ChatMessages(messages any) []Message {
	items, _ := messages.([]any)
	out := make([]Message, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" || role == "tool" {
			continue
		}
		out = append(out, Message{Role: role, Content: flattenContent(m["content"], "text")})
	}
	return out
}

// ResponsesMessages flattens a Responses API request: instructions become a
// system turn and each message item in input becomes a turn. A bare string
// input is a single user turn. Non-message items (function calls and
// outputs) are skipped.
func ResponsesMessages(instructions, input any) []Message {
	var out []Message
	if s, ok := instructions.(string); ok && s != "" {
		out = append(out, Message{Role: "system", Content: s})
	}
	switch in := input.(type) {
	case string:
		out = append(out, Message{Role: "user", Content: in})
	case []any:
		for _, item := range in {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := m["type"].(string); typ != "" && typ != "message" {
				continue
			}
			role, _ := m["role"].(string)
			if role == "" {
				continue
			}
			out = append(out, Message{Role: role, Content: flattenContent(m["content"], "input_text", "output_text")})
		}
	}
	return out
}

// ResponsesOutputText concatenates the assistant text from a Responses output
// array: every output_text and refusal part of every message item.
func ResponsesOutputText(output any) string {
	items, _ := output.([]any)
	var b strings.Builder
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != "message" {
			continue
		}
		text := flattenContent(m["content"], "output_text", "refusal")
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// flattenContent renders string or part-array content as text. Parts whose
// type is in textTypes contribute their text (or refusal); other parts are
// replaced with a bracketed placeholder.
func flattenContent(content any, textTypes ...string) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for i, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if i > 0 {
				b.WriteByte('\n')
			}
			typ, _ := p["type"].(string)
			if isText(typ, textTypes) {
				if text, ok := p["text"].(string); ok {
					b.WriteString(text)
				} else if refusal, ok := p["refusal"].(string); ok {
					b.WriteString(refusal)
				}
				continue
			}
			fmt.Fprintf(&b, "[%s]", typ)
		}
		return b.String()
	}
	return ""
}

func isText(typ string, textTypes []string) bool {
	for _, t := range textTypes {
		if typ == t {
			return true
		}
	}
	return false
}

// RequestMessages extracts the conversation history from a parsed request
// body for the given API path, or nil when the path is not classified.
func RequestMessages(path string, body map[string]any) []Message {
	switch path {
	case "/v1/chat/completions":
		return ChatMessages(body["messages"])
	case "/v1/responses":
		return ResponsesMessages(body["instructions"], body["input"])
	}
	return nil
}
