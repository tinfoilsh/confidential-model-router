package toolruntime

// Profile identifies one built-in tool family the router dials as an MCP server.
type Profile struct {
	Name            string // stable identifier used in detectors and logs
	ToolServerModel string // enclave manager model key to dial
}

// Descriptor captures everything the router needs to activate and drive a
// built-in tool profile: activation signals, aliasing, and prompt.
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

	// System prompt injected when active. tools is the set of model-facing
	// tool names available to the request.
	Prompt func(harmony bool, tools map[string]struct{}) string
}

// All profiles
var WebSearch = Profile{
	Name:            "web_search",
	ToolServerModel: "websearch",
}

// piiFilterModel is the catalog key for the privacy filter that the websearch
// service runs on outgoing queries. The router never dials it directly, but
// reads its published per-request price to report the fee in usage metrics.
const piiFilterModel = "pii-filter"

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
