package toolruntime

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/toolprofile"
	"github.com/tinfoilsh/confidential-model-router/toolruntime/citations"
)

// Descriptor captures everything the router needs to activate and drive a
// built-in tool profile: the activation signals, tool-name aliasing,
// system prompt, and per-request meta attachment.
type Descriptor struct {
	Profile toolprofile.Profile

	// ResponsesToolType is the /v1/responses tools[] entry type that
	// activates this profile (e.g. "web_search"). Empty = never
	// activated via /responses tools[].
	ResponsesToolType string

	// OptionsActive reports whether this profile should activate based
	// on parsed RouterOptions (the chat-completions path). Nil = never
	// activated via chat options.
	OptionsActive func(*RouterOptions) bool

	// Aliases maps the MCP server's tool name to the outward
	// (model-facing) name. Entries not in the map keep their name.
	// Nil = identity (no aliasing).
	Aliases map[string]string

	// Prompt returns the system prompt text injected when this profile
	// is active. Nil = no prompt fragment.
	Prompt func(harmony bool) string

	// AttachMeta lifts per-request options from RouterOptions onto the
	// session registry as mcp.Meta. Nil = no meta attachment.
	AttachMeta func(*sessionRegistry, *RouterOptions)
}

var profiles = []Descriptor{
	{
		Profile:           toolprofile.WebSearch,
		ResponsesToolType: "web_search",
		OptionsActive:     func(o *RouterOptions) bool { return o.WebSearch != nil },
		Aliases: map[string]string{
			mcpSearchToolName: routerSearchToolName,
			mcpFetchToolName:  routerFetchToolName,
		},
		Prompt: webSearchPrompt,
	},
	{
		Profile:           toolprofile.CodeExecution,
		ResponsesToolType: "code_execution",
		OptionsActive:     func(o *RouterOptions) bool { return o.CodeExecution != nil },
		Prompt:            func(harmony bool) string { return codeExecutionInstructions },
		AttachMeta:        attachCodeExecutionMeta,
	},
}

func descriptorForProfile(profileName string) *Descriptor {
	for i := range profiles {
		if profiles[i].Profile.Name == profileName {
			return &profiles[i]
		}
	}
	return nil
}

// DetectProfiles inspects an incoming request and returns the set of
// built-in tool profiles that should be activated for it.
func DetectProfiles(path string, opts *RouterOptions, body map[string]any) []toolprofile.Profile {
	var active []toolprofile.Profile
	seen := map[string]bool{}
	for _, d := range profiles {
		activated := false
		if d.OptionsActive != nil && d.OptionsActive(opts) {
			activated = true
		}
		if !activated && path == "/v1/responses" && d.ResponsesToolType != "" {
			if responsesToolTypePresent(body, d.ResponsesToolType) {
				activated = true
			}
		}
		if activated && !seen[d.Profile.Name] {
			active = append(active, d.Profile)
			seen[d.Profile.Name] = true
		}
	}
	return active
}

// responsesToolTypePresent reports whether the /v1/responses body
// carries a tools[] entry with the given type.
func responsesToolTypePresent(body map[string]any, toolType string) bool {
	tools, ok := body["tools"].([]any)
	if !ok {
		return false
	}
	for _, t := range tools {
		m, _ := t.(map[string]any)
		if typeVal, _ := m["type"].(string); typeVal == toolType {
			return true
		}
	}
	return false
}

// webSearchPrompt builds the system prompt for the web_search profile.
func webSearchPrompt(harmony bool) string {
	cite := citations.Instructions
	if harmony {
		cite = citations.HarmonyInstructions
	}
	return fmt.Sprintf("You may use the %s and %s tools when current web information would improve the answer. Use %s first to discover sources, then %s specific URLs only when you need deeper detail. %s %s %s", routerSearchToolName, routerFetchToolName, routerSearchToolName, routerFetchToolName, cite, toolEconomyInstructions, toolOutputWarning)
}

// attachCodeExecutionMeta lifts code-execution credentials from
// RouterOptions onto the session registry as mcp.Meta.
func attachCodeExecutionMeta(registry *sessionRegistry, opts *RouterOptions) {
	ce := opts.CodeExecution
	if ce == nil {
		return
	}
	metaBlock := map[string]any{
		"accessToken":        ce.AccessToken,
		"encryptionKey":      ce.EncryptionKey,
		"containerAuthToken": ce.ContainerAuthToken,
	}
	if ce.Uploads != nil {
		arr := make([]any, len(*ce.Uploads))
		for i, u := range *ce.Uploads {
			arr[i] = map[string]any{
				"fileAccessToken": u.FileAccessToken,
				"filename":        u.Filename,
				"sha256":          u.Sha256,
			}
		}
		metaBlock["uploads"] = arr
	}
	registry.metaByProfile[toolprofile.CodeExecution.Name] = mcp.Meta{
		toolprofile.CodeExecutionMetaKey: metaBlock,
	}
}
