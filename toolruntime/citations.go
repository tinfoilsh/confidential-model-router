package toolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/toolruntime/citations"
)

// publicToolErrorReason returns a short, opaque status string safe to
// ship to clients via `web_search_call.reason`.
const (
	publicToolErrorReasonString = "tool_error"
	blockedToolErrorReason      = "blocked_by_safety_filter"
)

func publicToolErrorReason(toolName string, err error) string {
	if err == nil {
		return ""
	}
	debugLogf("toolruntime: %s tool call failed: %v", toolName, err)
	if isToolCallBlocked(err) {
		return blockedToolErrorReason
	}
	return publicToolErrorReasonString
}

// isToolCallBlocked reports whether the MCP tool error came from a PII or
// prompt-injection safeguard.
func isToolCallBlocked(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "blocked by safety filter")
}

// failureStatusFor picks the web_search_call status string to surface for
// a non-nil MCP error.
func failureStatusFor(err error) string {
	if isToolCallBlocked(err) {
		return "blocked"
	}
	return "failed"
}

// toolResultErrorMessage extracts a human-readable error message from a
// CallToolResult whose `IsError` flag is set.
func toolResultErrorMessage(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var parts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			if trimmed := strings.TrimSpace(textContent.Text); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func callTool(ctx context.Context, session *mcp.ClientSession, name string, arguments map[string]any, state *citations.State) (string, error) {
	start := time.Now()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		debugLogf("toolruntime:mcp.error tool=%s elapsed=%s args=%s err=%v", name, time.Since(start), debugPreview(arguments, 400), err)
		return "", err
	}
	if result.IsError {
		message := toolResultErrorMessage(result)
		debugLogf("toolruntime:mcp.tool_error tool=%s elapsed=%s args=%s message=%q", name, time.Since(start), debugPreview(arguments, 400), message)
		if message == "" {
			message = "tool call failed"
		}
		return "", errors.New(message)
	}
	if debugEnabled {
		hasStructured := result.StructuredContent != nil
		textParts := 0
		for _, content := range result.Content {
			if _, ok := content.(*mcp.TextContent); ok {
				textParts++
			}
		}
		debugLogf("toolruntime:mcp.ok tool=%s elapsed=%s structured=%t text_parts=%d structured_preview=%s",
			name, time.Since(start), hasStructured, textParts, debugPreview(result.StructuredContent, 500))
	}
	if result.StructuredContent != nil {
		if formatted := formatStructuredToolOutput(name, result.StructuredContent, state); formatted != "" {
			return formatted, nil
		}
		body, err := json.Marshal(result.StructuredContent)
		if err == nil {
			return string(body), nil
		}
	}
	var parts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, textContent.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func formatStructuredToolOutput(name string, raw any, state *citations.State) string {
	content, _ := raw.(map[string]any)
	if len(content) == 0 {
		return ""
	}

	switch {
	case isRouterSearchToolName(name):
		return citations.FormatSearchOutput(content["results"], state)
	case isRouterFetchToolName(name):
		if formatted := citations.FormatFetchOutput(content["pages"], state); formatted != "" {
			return formatted
		}
		return citations.FormatFetchFailures(content["results"])
	default:
		return ""
	}
}

// toolOutputSourcesToToolCallSources converts citations.ToolOutputSource
// to the toolCallSource type used by the events/progress system.
func toolOutputSourcesToToolCallSources(sources []citations.ToolOutputSource) []toolCallSource {
	if len(sources) == 0 {
		return nil
	}
	result := make([]toolCallSource, len(sources))
	for i, s := range sources {
		result[i] = toolCallSource{url: s.URL, title: s.Title}
	}
	return result
}
