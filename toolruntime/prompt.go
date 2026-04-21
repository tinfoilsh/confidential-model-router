package toolruntime

import (
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func prependChatPrompt(prompt *mcp.GetPromptResult, raw any) []any {
	messages, _ := raw.([]any)
	prefix := chatPromptPrefix(prompt)
	if len(prefix) == 0 {
		return messages
	}
	return append(prefix, messages...)
}

func prependResponsesPrompt(prompt *mcp.GetPromptResult, raw any) any {
	items := normalizeResponsesInput(raw)
	prefix := responsePromptPrefix(prompt)
	if len(prefix) == 0 {
		return items
	}
	return append(prefix, items...)
}

func buildRouterPrompt() *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Description: "Instructions for router-owned web search tool use.",
		Messages: []*mcp.PromptMessage{
			{
				Role: "system",
				Content: &mcp.TextContent{
					Text: "You may use the search and fetch tools when current web information would improve the answer. Use search first to discover sources, then fetch specific URLs only when you need deeper detail. " + citationInstructions + " " + toolOutputWarning + " " + toolEconomyInstructions,
				},
			},
		},
	}
}

func forcedFinalChatRequest(reqBody map[string]any) map[string]any {
	finalBody := cloneJSONMap(reqBody)
	messages, _ := finalBody["messages"].([]any)
	finalBody["messages"] = append(messages, map[string]any{
		"role":    "system",
		"content": finalAnswerInstructionText,
	})
	delete(finalBody, "tools")
	// Every field that could otherwise require or steer a tool call must
	// be stripped too; leaving tool_choice:"required" on a request that
	// carries no tools is a protocol-level contradiction that upstream
	// will reject or loop on. function_call is the legacy Chat
	// Completions equivalent and is removed for the same reason.
	delete(finalBody, "tool_choice")
	delete(finalBody, "function_call")
	finalBody["parallel_tool_calls"] = false
	return finalBody
}

func forcedFinalResponsesRequest(reqBody map[string]any) map[string]any {
	finalBody := cloneJSONMap(reqBody)
	finalBody["input"] = append(forcedFinalResponseInput(finalBody["input"]), map[string]any{
		"type": "message",
		"role": "system",
		"content": []map[string]any{
			{
				"type": "input_text",
				"text": finalAnswerInstructionText,
			},
		},
	})
	delete(finalBody, "tools")
	delete(finalBody, "tool_choice")
	finalBody["parallel_tool_calls"] = false
	return finalBody
}

// forcedFinalResponseInput preserves the full conversation history for
// the forced-final turn: user/assistant messages, router-issued
// function_call items, and their matching function_call_output items
// all ride through unchanged. Upstream accepts this shape even with
// an empty tools array because the Responses API binds call_id to
// the prior function_call in input, not to any currently-listed
// tool.
//
// We deliberately do NOT rewrite function_call_output into a
// message-role synthesis of "Tool result: <text>": doing so would
// either bury tool content in the assistant-authoritative system
// role (a prompt-injection footgun, since tool outputs include
// scraped web content) or duplicate it as a free-floating user
// turn disconnected from its originating call. Leaving the items
// typed and call_id-linked is the only shape that preserves both
// the trust boundary (tool output stays tool output) and the
// conversation's structural integrity.
func forcedFinalResponseInput(raw any) []any {
	input := normalizeResponsesInput(raw)
	finalInput := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}

		switch stringValue(item["type"]) {
		case "message", "function_call", "function_call_output":
			finalInput = append(finalInput, rawItem)
		}
	}
	return finalInput
}

func normalizeResponsesInput(raw any) []any {
	switch value := raw.(type) {
	case nil:
		return []any{}
	case []any:
		return value
	case []map[string]any:
		items := make([]any, 0, len(value))
		for _, item := range value {
			items = append(items, item)
		}
		return items
	case string:
		if strings.TrimSpace(value) == "" {
			return []any{}
		}
		return []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{
						"type": "input_text",
						"text": value,
					},
				},
			},
		}
	default:
		return []any{raw}
	}
}

func normalizeResponsesOutputItems(items []any) []any {
	normalized := make([]any, 0, len(items))
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}

		if stringValue(item["type"]) != "mcp_call" {
			normalized = append(normalized, rawItem)
			continue
		}

		normalized = append(normalized, map[string]any{
			"type":      "function_call",
			"call_id":   firstNonEmptyString(item["call_id"], item["id"]),
			"name":      item["name"],
			"arguments": item["arguments"],
		})
	}
	return normalized
}

func promptMessages(prompt *mcp.GetPromptResult) []any {
	if prompt == nil {
		return nil
	}
	result := make([]any, 0, len(prompt.Messages))
	for _, message := range prompt.Messages {
		textContent, ok := message.Content.(*mcp.TextContent)
		if !ok || strings.TrimSpace(textContent.Text) == "" {
			continue
		}
		result = append(result, map[string]any{
			"role":    message.Role,
			"content": textContent.Text,
		})
	}
	return result
}

func routedToolDescription(tool *mcp.Tool) string {
	if tool == nil {
		return ""
	}
	description := strings.TrimSpace(tool.Description)
	if description == "" {
		description = fmt.Sprintf("Use the %s tool when it would improve the answer.", tool.Name)
	}
	return fmt.Sprintf(
		"%s Today is %s. Each result includes the source URL and title; cite them inline using standard markdown link syntax, where the label is in square brackets and the URL follows in parentheses, for example [Page title](https://example.com/page). Do not wrap the link in any additional brackets. %s %s %s",
		description,
		time.Now().Format(currentDateTimeFormat),
		citationInstructions,
		toolOutputWarning,
		toolEconomyInstructions,
	)
}

func chatPromptPrefix(prompt *mcp.GetPromptResult) []any {
	basePromptMessages := promptMessages(prompt)
	result := make([]any, 0, len(basePromptMessages)+1)
	if contextMessage := buildContextMessage(); contextMessage != "" {
		result = append(result, map[string]any{
			"role":    "system",
			"content": contextMessage,
		})
	}
	result = append(result, basePromptMessages...)
	return result
}

func promptInputItems(prompt *mcp.GetPromptResult) []any {
	if prompt == nil {
		return nil
	}
	result := make([]any, 0, len(prompt.Messages))
	for _, message := range prompt.Messages {
		textContent, ok := message.Content.(*mcp.TextContent)
		if !ok || strings.TrimSpace(textContent.Text) == "" {
			continue
		}
		result = append(result, map[string]any{
			"type": "message",
			"role": message.Role,
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": textContent.Text,
				},
			},
		})
	}
	return result
}

func responsePromptPrefix(prompt *mcp.GetPromptResult) []any {
	basePromptItems := promptInputItems(prompt)
	result := make([]any, 0, len(basePromptItems)+1)
	if contextMessage := buildContextMessage(); contextMessage != "" {
		result = append(result, map[string]any{
			"type": "message",
			"role": "system",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": contextMessage,
				},
			},
		})
	}
	result = append(result, basePromptItems...)
	return result
}

func buildContextMessage() string {
	return fmt.Sprintf(
		"Current date and time: %s. If the user asks about \"today\", \"latest\", or other time-sensitive topics, interpret them relative to this timestamp and prioritize the freshest tool results.",
		time.Now().Format(currentDateTimeFormat),
	)
}
