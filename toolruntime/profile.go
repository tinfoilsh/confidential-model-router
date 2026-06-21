package toolruntime

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Profile identifies one built-in tool family the router dials as an MCP server.
type Profile struct {
	Name            string // stable identifier used in detectors and logs
	ToolServerModel string // enclave manager model key to dial
}

// Descriptor captures everything the router needs to activate and drive a
// built-in tool profile: activation signals, aliasing, prompt, and per-request meta.
type Descriptor struct {
	Profile Profile

	// ResponsesToolType is the /v1/responses tools[] entry type that
	// activates this profile (e.g. "web_search").
	ResponsesToolType string

	// OptionsActive reports whether this profile should activate based
	// on parsed RouterOptions (the chat-completions path).
	OptionsActive func(*RouterOptions) bool

	// Aliases maps the MCP server's tool name to the outward
	// (model-facing) name. Entries not in the map keep their name.
	Aliases map[string]string

	// System prompt injected when active
	Prompt func(harmony bool) string

	// Lifts per-request options from RouterOptions onto the session registry as mcp.Meta
	AttachMeta func(*sessionRegistry, *RouterOptions)
}

// All profiles
var WebSearch = Profile{
	Name:            "web_search",
	ToolServerModel: "websearch",
}

var CodeExecution = Profile{
	Name:            "code_execution",
	ToolServerModel: "code-execution",
}

var profiles = []Descriptor{
	{
		Profile:           WebSearch,
		ResponsesToolType: "web_search",
		OptionsActive:     func(o *RouterOptions) bool { return o.WebSearch != nil },
		Aliases: map[string]string{
			mcpSearchToolName: routerSearchToolName,
			mcpFetchToolName:  routerFetchToolName,
		},
		Prompt: webSearchPrompt,
	},
	{
		Profile:           CodeExecution,
		ResponsesToolType: "code_execution",
		OptionsActive:     func(o *RouterOptions) bool { return o.CodeExecution != nil },
		Prompt:            func(harmony bool) string { return codeExecutionInstructions },
		AttachMeta:        attachCodeExecutionMeta,
	},
}

const CodeExecutionMetaKey = "tinfoil_code_exec"

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
func DetectProfiles(path string, opts *RouterOptions, body map[string]any) []Profile {
	var active []Profile
	seen := map[string]bool{}
	for _, d := range profiles {
		activated := false
		if path == "/v1/chat/completions" && d.OptionsActive != nil && d.OptionsActive(opts) {
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
	registry.metaByProfile[CodeExecution.Name] = mcp.Meta{
		CodeExecutionMetaKey: metaBlock,
	}
}
