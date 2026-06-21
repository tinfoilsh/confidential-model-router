package toolruntime

import "github.com/modelcontextprotocol/go-sdk/mcp"

const (
	routerSearchToolName = "router_search"
	routerFetchToolName  = "router_fetch"

	mcpSearchToolName  = "search"
	mcpFetchToolName   = "fetch"
	mcpPresentToolName = "present"
)

func outwardRouterToolName(profileName, toolName string) string {
	d := descriptorForProfile(profileName)
	if d == nil || d.Aliases == nil {
		return toolName
	}
	if outward, ok := d.Aliases[toolName]; ok {
		return outward
	}
	return toolName
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
	for _, d := range profiles {
		for mcp, outward := range d.Aliases {
			if outward == toolName {
				return mcp
			}
		}
	}
	return toolName
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
