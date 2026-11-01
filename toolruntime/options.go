package toolruntime

import (
	"fmt"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// Router-only body fields. Each is an OpenAI-compatible-but-Tinfoil-specific
// options blob: the model router reads and acts on them, then strips them
// from the body before forwarding to the model enclave. The data flows back
// in via RouterOptions at the MCP boundary, so the upstream model never sees
// these fields but the MCP servers still get the caller's preferences.
const (
	fieldWebSearchOptions = "web_search_options"
	fieldPIICheckOptions  = "pii_check_options"
)

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
	WebSearch *WebSearchOptions
	PIICheck  *PIICheckOptions
}

// ExtractRouterOptions pulls every router-only options field off the
// request body in place. The body afterwards is the plain OpenAI-spec
// shape that gets forwarded to the model enclave.
//
// web_search_options and pii_check_options are lifted into typed
// wrappers and the body field is stripped. The only web search setting
// validated here is external_web_access, on web_search_options and on
// Responses web_search tools, so a malformed opt-out of live web access
// is rejected rather than ignored.
func ExtractRouterOptions(body map[string]any) (*RouterOptions, error) {
	opts := &RouterOptions{}

	if raw, ok := body[fieldWebSearchOptions]; ok {
		ws, _ := raw.(map[string]any)
		if err := validateExternalWebAccess(ws, fieldWebSearchOptions); err != nil {
			return nil, err
		}
		opts.WebSearch = &WebSearchOptions{Raw: ws}
		delete(body, fieldWebSearchOptions)
	}

	if err := validateWebSearchTools(body); err != nil {
		return nil, err
	}

	if _, ok := body[fieldPIICheckOptions]; ok {
		opts.PIICheck = &PIICheckOptions{}
		delete(body, fieldPIICheckOptions)
	}

	return opts, nil
}

// optionsParamError returns a 400 invalid_request_error for a router-only
// options field, with param set to the field's JSON path.
func optionsParamError(param string, detail string) *manager.APIError {
	return manager.ErrInvalidRequest.WithParam(param).WithMessage(manager.ErrMsgInvalidParam, param, detail)
}

// validateExternalWebAccess rejects a non-boolean external_web_access on the
// web search settings found at param. Absent and null both mean the default.
func validateExternalWebAccess(settings map[string]any, param string) error {
	raw := settings[fieldExternalWebAccess]
	if raw == nil {
		return nil
	}
	if _, ok := raw.(bool); !ok {
		return optionsParamError(param+"."+fieldExternalWebAccess, "must be a boolean")
	}
	return nil
}

// validateWebSearchTools applies validateExternalWebAccess to every
// Responses web_search tool entry.
func validateWebSearchTools(body map[string]any) error {
	toolType := descriptorForProfile(WebSearch.Name).ResponsesToolType
	tools, _ := body["tools"].([]any)
	for i, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		if stringValue(tool["type"]) != toolType {
			continue
		}
		if err := validateExternalWebAccess(tool, fmt.Sprintf("tools[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}
