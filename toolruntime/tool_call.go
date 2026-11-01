package toolruntime

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolCall struct {
	id        string
	name      string
	arguments map[string]any
}

func parseChatToolCalls(response map[string]any) (map[string]any, []toolCall) {
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, nil
	}
	rawCalls, _ := message["tool_calls"].([]any)
	toolCalls := make([]toolCall, 0, len(rawCalls))
	for _, rawCall := range rawCalls {
		callMap, _ := rawCall.(map[string]any)
		functionMap, _ := callMap["function"].(map[string]any)
		args := parseArguments(functionMap["arguments"])
		toolCalls = append(toolCalls, toolCall{
			id:        stringValue(callMap["id"]),
			name:      stringValue(functionMap["name"]),
			arguments: args,
		})
	}
	return message, toolCalls
}

func parseResponsesToolCalls(response map[string]any) []toolCall {
	output, _ := response["output"].([]any)
	var result []toolCall
	for _, item := range output {
		itemMap, _ := item.(map[string]any)
		itemType := stringValue(itemMap["type"])
		if itemType != "function_call" && itemType != "mcp_call" {
			continue
		}
		callID := stringValue(itemMap["call_id"])
		if callID == "" {
			callID = stringValue(itemMap["id"])
		}
		name := stringValue(itemMap["name"])
		args := parseArguments(itemMap["arguments"])
		result = append(result, toolCall{
			id:        callID,
			name:      name,
			arguments: args,
		})
	}
	return result
}

func parseArguments(raw any) map[string]any {
	switch value := raw.(type) {
	case string:
		var parsed map[string]any
		raw, _ := sanitizeToolCallArgumentsJSON([]byte(value))
		if json.Unmarshal(raw, &parsed) == nil {
			return parsed
		}
	case map[string]any:
		return value
	}
	return map[string]any{}
}

func ownedToolNames(tools []*mcp.Tool) map[string]struct{} {
	result := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name == "" {
			continue
		}
		result[tool.Name] = struct{}{}
	}
	return result
}

func splitToolCalls(ownedTools map[string]struct{}, toolCalls []toolCall) ([]toolCall, []toolCall) {
	routerToolCalls := make([]toolCall, 0, len(toolCalls))
	clientToolCalls := make([]toolCall, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if _, ok := ownedTools[toolCall.name]; ok {
			routerToolCalls = append(routerToolCalls, toolCall)
			continue
		}
		clientToolCalls = append(clientToolCalls, toolCall)
	}
	return routerToolCalls, clientToolCalls
}

func existingTools(raw any) []any {
	tools, _ := raw.([]any)
	if tools == nil {
		return []any{}
	}
	return tools
}

func chatTools(tools []*mcp.Tool) []any {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name == "" {
			continue
		}
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": routedToolDescription(tool),
				"parameters":  tool.InputSchema,
			},
		})
	}
	return result
}

func responseTools(tools []*mcp.Tool) []any {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name == "" {
			continue
		}
		result = append(result, map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": routedToolDescription(tool),
			"parameters":  tool.InputSchema,
		})
	}
	return result
}

// routerOwnedToolTypes is the set of tool type values in the /responses
// tools array that the router resolves internally via MCP sessions. These
// placeholder entries are stripped from the upstream request and replaced
// with the concrete function definitions the MCP servers advertise.
var routerOwnedToolTypes = map[string]bool{
	"web_search": true,
}

func replaceRouterOwnedResponsesTools(raw any, replacements []any) []any {
	tools, _ := raw.([]any)
	if len(tools) == 0 {
		return replacements
	}
	result := make([]any, 0, len(tools)+len(replacements))
	injected := false
	for _, tool := range tools {
		toolMap, _ := tool.(map[string]any)
		if routerOwnedToolTypes[stringValue(toolMap["type"])] {
			if !injected {
				result = append(result, replacements...)
				injected = true
			}
			continue
		}
		result = append(result, tool)
	}
	return result
}
