package toolprofile

// Profile identifies one built-in tool family and the MCP model name
// the router dials to service its tool calls. The Name is the stable
// identifier used in request detectors and logs; ToolServerModel
// keys into the enclave manager's model table and the
// LOCAL_MCP_ENDPOINT_<MODEL> debug override.
type Profile struct {
	Name            string
	ToolServerModel string
}

// WebSearch is activated by `web_search_options` on /chat/completions
// and by a `{"type": "web_search"}` entry in /responses `tools`.
var WebSearch = Profile{
	Name:            "web_search",
	ToolServerModel: "websearch",
}

// CodeExecution is activated by `code_execution_options` on
// /chat/completions and by a `{"type": "code_execution"}` entry in
// /responses `tools`.
var CodeExecution = Profile{
	Name:            "code_execution",
	ToolServerModel: "code-execution",
}
