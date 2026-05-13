package toolruntime

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/toolprofile"
)

// Router-only body fields. Each is an OpenAI-compatible-but-Tinfoil-specific
// options blob: the model router reads and acts on them, then strips them
// from the body before forwarding to the model enclave. The data flows back
// in via RouterOptions at the MCP boundary, so the upstream model never sees
// these fields but the MCP servers still get the caller's preferences.
const (
	fieldCodeExecutionOptions = "code_execution_options"
	fieldWebSearchOptions     = "web_search_options"
	fieldPIICheckOptions      = "pii_check_options"
)

type CodeExecutionOptions struct {
	AccessToken        string
	EncryptionKey      string
	ContainerAuthToken string
}

// WebSearchOptions wraps the caller's web_search_options block so it
// survives the strip-from-body step at the router edge. Raw is the
// inner object exactly as the caller sent it; field-level parsing
// (search_context_size, user_location, filters, advanced knobs)
// happens in parseChatWebSearchOptions at the MCP boundary.
type WebSearchOptions struct {
	Raw map[string]any
}

// PIICheckOptions is intentionally empty today. The presence of a
// non-nil pointer is the signal that the caller opted in; the block's
// contents are reserved for future per-request tuning.
type PIICheckOptions struct{}

type RouterOptions struct {
	CodeExecution *CodeExecutionOptions
	WebSearch     *WebSearchOptions
	PIICheck      *PIICheckOptions
}

// ExtractRouterOptions pulls every router-only options field off the
// request body in place. The body afterwards is the plain OpenAI-spec
// shape that gets forwarded to the model enclave.
//
// Validation is eager: if code_execution_options is present, all three
// credential subfields must be non-empty strings. A missing field or
// malformed shape returns an error so the router responds 400 with a
// clear message instead of deferring to the orchestrator.
//
// web_search_options and pii_check_options have no validation surface
// today; their presence and (for web_search) inner payload are lifted
// into typed wrappers and the body field is stripped.
func ExtractRouterOptions(body map[string]any) (*RouterOptions, error) {
	opts := &RouterOptions{}

	if raw, ok := body[fieldCodeExecutionOptions]; ok {
		ce, err := parseCodeExecutionOptions(raw)
		if err != nil {
			return nil, err
		}
		opts.CodeExecution = ce
		delete(body, fieldCodeExecutionOptions)
	}

	if raw, ok := body[fieldWebSearchOptions]; ok {
		ws, _ := raw.(map[string]any)
		opts.WebSearch = &WebSearchOptions{Raw: ws}
		delete(body, fieldWebSearchOptions)
	}

	if _, ok := body[fieldPIICheckOptions]; ok {
		opts.PIICheck = &PIICheckOptions{}
		delete(body, fieldPIICheckOptions)
	}

	return opts, nil
}

func parseCodeExecutionOptions(raw any) (*CodeExecutionOptions, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("code_execution_options must be an object")
	}
	ce := &CodeExecutionOptions{
		AccessToken:        stringField(m, "accessToken"),
		EncryptionKey:      stringField(m, "encryptionKey"),
		ContainerAuthToken: stringField(m, "containerAuthToken"),
	}
	if ce.AccessToken == "" {
		return nil, fmt.Errorf("code_execution_options.accessToken is required")
	}
	if ce.EncryptionKey == "" {
		return nil, fmt.Errorf("code_execution_options.encryptionKey is required")
	}
	if ce.ContainerAuthToken == "" {
		return nil, fmt.Errorf("code_execution_options.containerAuthToken is required")
	}
	return ce, nil
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// attachRouterOptionsMeta lifts per-request secrets from RouterOptions onto
// the session registry as mcp.Meta
func attachRouterOptionsMeta(registry *sessionRegistry, opts *RouterOptions) {
	if registry == nil || opts == nil {
		return
	}
	if ce := opts.CodeExecution; ce != nil {
		registry.metaByProfile[toolprofile.CodeExecution.Name] = mcp.Meta{
			toolprofile.CodeExecutionMetaKey: map[string]any{
				"accessToken":        ce.AccessToken,
				"encryptionKey":      ce.EncryptionKey,
				"containerAuthToken": ce.ContainerAuthToken,
			},
		}
	}
}
