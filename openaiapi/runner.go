package openaiapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

type ProxyRequest struct {
	EffectiveModel string
	Body           []byte
}

type WebSearchRouter interface {
	Prepare(req *Request) (*ProxyRequest, error)
}

type Runner struct {
	webSearch WebSearchRouter
	tools     []Tool
}

func NewRunner(webSearch WebSearchRouter, tools ...Tool) *Runner {
	cloned := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if tool != nil {
			cloned = append(cloned, tool)
		}
	}
	return &Runner{
		webSearch: webSearch,
		tools:     cloned,
	}
}

func (r *Runner) PrepareWebSearch(req *Request) (*ProxyRequest, error) {
	if r == nil || r.webSearch == nil || req == nil {
		return nil, nil
	}
	return r.webSearch.Prepare(req)
}

func (r *Runner) Activate(req *Request) (*ToolSet, error) {
	if r == nil || req == nil {
		return nil, nil
	}

	toolSet := newToolSet()
	for _, tool := range r.tools {
		params, session, err := tool.GetParams(req, req.Kind)
		if err != nil {
			_ = toolSet.Close(context.Background())
			return nil, err
		}
		if params == nil && session == nil {
			continue
		}
		if params == nil || session == nil {
			_ = toolSet.Close(context.Background())
			return nil, fmt.Errorf("active tool %q must provide params and a session", tool.ID())
		}
		if err := toolSet.add(tool.ID(), params, session); err != nil {
			_ = toolSet.Close(context.Background())
			return nil, err
		}
	}

	if toolSet.Empty() {
		return nil, nil
	}
	if err := validateUserToolCollisions(req, toolSet); err != nil {
		_ = toolSet.Close(context.Background())
		return nil, err
	}
	return toolSet, nil
}

func (r *Runner) BuildRequestBody(req *Request, toolSet *ToolSet) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("request is required")
	}
	if toolSet == nil || toolSet.Empty() {
		return req.BodyBytes()
	}

	switch req.Kind {
	case EndpointChatCompletions:
		return buildChatRequestBody(req, toolSet)
	case EndpointResponses:
		return buildResponsesRequestBody(req, toolSet)
	default:
		return req.BodyBytes()
	}
}

func buildChatRequestBody(req *Request, toolSet *ToolSet) ([]byte, error) {
	fields := cloneRawFields(req.RawFields)
	for field := range toolSet.stripFields {
		delete(fields, field)
	}

	existing, err := rawArray(fields["tools"])
	if err != nil {
		return nil, fmt.Errorf("invalid parameter: 'tools' must be an array")
	}
	for _, spec := range toolSet.ChatTools() {
		existing = append(existing, cloneRawMessage(spec))
	}
	if len(existing) == 0 {
		delete(fields, "tools")
	} else {
		encoded, err := json.Marshal(existing)
		if err != nil {
			return nil, err
		}
		fields["tools"] = encoded
	}

	return marshalRawFields(fields)
}

func buildResponsesRequestBody(req *Request, toolSet *ToolSet) ([]byte, error) {
	fields := cloneRawFields(req.RawFields)
	for field := range toolSet.stripFields {
		delete(fields, field)
	}

	toolsRaw, err := rawArray(fields["tools"])
	if err != nil {
		return nil, fmt.Errorf("invalid parameter: 'tools' must be an array")
	}

	filtered := make([]json.RawMessage, 0, len(toolsRaw)+len(toolSet.ResponseTools()))
	hasOtherTools := false
	if req.Responses != nil {
		for idx, tool := range req.Responses.Params.Tools {
			if toolSet.InterceptsResponseToolType(responsesBuiltinToolType(tool)) {
				continue
			}
			hasOtherTools = true
			if idx < len(toolsRaw) {
				filtered = append(filtered, cloneRawMessage(toolsRaw[idx]))
			}
		}
	}
	for _, spec := range toolSet.ResponseTools() {
		filtered = append(filtered, cloneRawMessage(spec))
	}

	if len(filtered) == 0 {
		delete(fields, "tools")
	} else {
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return nil, err
		}
		fields["tools"] = encoded
	}

	if err := normalizeInterceptedResponsesToolChoice(fields, toolSet, hasOtherTools); err != nil {
		return nil, err
	}
	return marshalRawFields(fields)
}

func normalizeInterceptedResponsesToolChoice(fields map[string]json.RawMessage, toolSet *ToolSet, hasOtherTools bool) error {
	if toolSet == nil || toolSet.Empty() {
		return nil
	}

	rawChoice, exists := fields["tool_choice"]
	if !exists {
		return nil
	}
	trimmed := strings.TrimSpace(string(rawChoice))
	if trimmed == "" || trimmed == "null" {
		delete(fields, "tool_choice")
		return nil
	}

	if len(trimmed) > 0 && trimmed[0] == '"' {
		var mode string
		if err := json.Unmarshal(rawChoice, &mode); err != nil {
			return fmt.Errorf("invalid parameter: 'tool_choice' must be a string or object")
		}
		if strings.TrimSpace(mode) == "required" {
			fields["tool_choice"] = json.RawMessage(`"auto"`)
		}
		return nil
	}

	var choice map[string]any
	if err := json.Unmarshal(rawChoice, &choice); err != nil {
		return fmt.Errorf("invalid parameter: 'tool_choice' must be a string or object")
	}

	choiceType := strings.TrimSpace(jsonString(choice["type"]))
	choiceName := ""
	if function, ok := choice["function"].(map[string]any); ok {
		choiceName = strings.TrimSpace(jsonString(function["name"]))
	}
	if choiceName == "" {
		choiceName = strings.TrimSpace(jsonString(choice["name"]))
	}

	targetsBuiltin := toolSet.HasCallName(choiceType) || (choiceType == "function" && toolSet.HasCallName(choiceName))
	switch {
	case targetsBuiltin && !hasOtherTools:
		fields["tool_choice"] = json.RawMessage(`"auto"`)
		return nil
	case targetsBuiltin && hasOtherTools:
		return fmt.Errorf("unsupported responses tool_choice for intercepted built-in request")
	case hasOtherTools:
		fields["tool_choice"] = json.RawMessage(`"auto"`)
		return nil
	default:
		return fmt.Errorf("unsupported responses tool_choice for intercepted built-in request")
	}
}

func validateUserToolCollisions(req *Request, toolSet *ToolSet) error {
	if req == nil || toolSet == nil || toolSet.Empty() {
		return nil
	}
	for _, name := range collectUserToolNames(req) {
		if toolSet.HasCallName(name) {
			return fmt.Errorf("tool name collision with reserved tool %q", name)
		}
	}
	return nil
}

func collectUserToolNames(req *Request) []string {
	if req == nil {
		return nil
	}

	names := map[string]struct{}{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" {
			names[name] = struct{}{}
		}
	}

	switch req.Kind {
	case EndpointChatCompletions:
		if req.Chat != nil {
			for _, tool := range req.Chat.Params.Tools {
				if tool.OfFunction != nil {
					add(tool.OfFunction.Function.Name)
				}
			}
		}
		functionsRaw, err := rawArray(req.RawFields["functions"])
		if err == nil {
			for _, rawFunction := range functionsRaw {
				fields, err := rawObjectFields(rawFunction)
				if err != nil || fields == nil {
					continue
				}
				var name string
				if err := json.Unmarshal(fields["name"], &name); err == nil {
					add(name)
				}
			}
		}
	case EndpointResponses:
		if req.Responses != nil {
			for _, tool := range req.Responses.Params.Tools {
				if tool.OfFunction != nil {
					add(tool.OfFunction.Name)
				}
			}
		}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	return result
}

func responsesBuiltinToolType(tool any) string {
	switch typed := tool.(type) {
	case interface{ GetType() *string }:
		return strings.TrimSpace(stringValue(typed.GetType()))
	default:
		return ""
	}
}

func (r *Runner) HandleJSON(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, toolSet *ToolSet, body []byte, invoker *manager.UpstreamInvoker) error {
	if toolSet == nil || toolSet.Empty() || invoker == nil {
		return nil
	}

	switch req.Kind {
	case EndpointChatCompletions:
		return r.handleChatJSON(ctx, w, httpReq, req, toolSet, body, invoker)
	case EndpointResponses:
		return r.handleResponsesJSON(ctx, w, httpReq, req, toolSet, body, invoker)
	default:
		return nil
	}
}

func (r *Runner) HandleStream(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, toolSet *ToolSet, body []byte, invoker *manager.UpstreamInvoker) error {
	if toolSet == nil || toolSet.Empty() || invoker == nil {
		return nil
	}

	switch req.Kind {
	case EndpointChatCompletions:
		return r.handleChatStream(ctx, w, httpReq, req, toolSet, body, invoker)
	case EndpointResponses:
		return r.handleResponsesStream(ctx, w, httpReq, req, toolSet, body, invoker)
	default:
		return nil
	}
}
