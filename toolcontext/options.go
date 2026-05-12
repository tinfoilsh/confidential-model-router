package toolcontext

import "fmt"

// Router-only body fields. Each is an OpenAI-compatible-but-Tinfoil-specific
// options blob: the model router reads and acts on them, then strips them
// from the body before forwarding to the model enclave.
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

type RouterOptions struct {
	CodeExecution   *CodeExecutionOptions
	WebSearchActive bool
	PIICheckActive  bool
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
// web_search_options and pii_check_options have no credential payload
// today; their presence is recorded as a boolean and the field stripped.
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

	if _, ok := body[fieldWebSearchOptions]; ok {
		opts.WebSearchActive = true
		delete(body, fieldWebSearchOptions)
	}

	if _, ok := body[fieldPIICheckOptions]; ok {
		opts.PIICheckActive = true
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
