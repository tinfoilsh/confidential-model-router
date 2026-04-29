package toolruntime

import "github.com/modelcontextprotocol/go-sdk/mcp"

const (
	routerSearchToolName = "router_search"
	routerFetchToolName  = "router_fetch"

	mcpSearchToolName = "search"
	mcpFetchToolName  = "fetch"

	mcpPresentToolName = "present"

	webSearchProfileName = "web_search"
)

func outwardRouterToolName(profileName, toolName string) string {
	if profileName != webSearchProfileName {
		return toolName
	}
	switch toolName {
	case mcpSearchToolName:
		return routerSearchToolName
	case mcpFetchToolName:
		return routerFetchToolName
	default:
		return toolName
	}
}

func outwardRouterTool(profileName string, tool *mcp.Tool) *mcp.Tool {
	if tool == nil {
		return nil
	}
	outwardName := outwardRouterToolName(profileName, tool.Name)
	if outwardName == tool.Name {
		return tool
	}
	cloned := *tool
	cloned.Name = outwardName
	return &cloned
}

func dispatchRouterToolName(toolName string) string {
	switch toolName {
	case routerSearchToolName:
		return mcpSearchToolName
	case routerFetchToolName:
		return mcpFetchToolName
	default:
		return toolName
	}
}

func isRouterSearchToolName(toolName string) bool {
	return dispatchRouterToolName(toolName) == mcpSearchToolName
}

func isRouterFetchToolName(toolName string) bool {
	return dispatchRouterToolName(toolName) == mcpFetchToolName
}

func isPresentTool(toolName string) bool {
	return dispatchRouterToolName(toolName) == mcpPresentToolName
}
