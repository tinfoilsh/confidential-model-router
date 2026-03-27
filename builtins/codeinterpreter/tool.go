package codeinterpreter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"github.com/tinfoilsh/confidential-model-router/openaiapi"
)

const (
	ToolName        = "code_interpreter"
	toolDescription = "Execute Python code in a sandbox container."
)

type Config struct {
	ControlPlaneURL string
	Repo            string
	ExecTimeout     time.Duration
}

type Tool struct {
	controlPlaneURL     string
	sandboxBootstrapper sandboxBootstrapper
	sandboxSpec         SandboxSpec
}

type runtime interface {
	Execute(ctx context.Context, callID, rawArgs string) (Result, error)
	Close(ctx context.Context) error
}

type sandboxRuntime struct {
	manager *SandboxManager
}

type toolSession struct {
	newRuntime     func() (runtime, error)
	includeOutputs bool
	runtime        runtime
	mu             sync.Mutex
}

func newToolSession(tool *Tool, callerAPIKey string, session *Session, includeOutputs bool) *toolSession {
	return &toolSession{
		// The prepared executor owns the caller API key for this request so the
		// eventual runtime and sandbox controlplane client are authorized as that caller.
		newRuntime: func() (runtime, error) {
			return tool.newRuntime(callerAPIKey, session)
		},
		includeOutputs: includeOutputs,
	}
}

func New(cfg Config) (*Tool, error) {
	tool := &Tool{}

	if strings.TrimSpace(cfg.Repo) != "" {
		if strings.TrimSpace(cfg.ControlPlaneURL) == "" {
			return nil, fmt.Errorf("control plane url is required when managed sandboxes are enabled")
		}
		tool.controlPlaneURL = cfg.ControlPlaneURL
		tool.sandboxBootstrapper = NewSandboxBootstrapper(cfg.ExecTimeout)
		tool.sandboxSpec = SandboxSpec{
			Workload:   sandboxWorkloadCodeInterpreter,
			SourceRepo: cfg.Repo,
		}
	}

	return tool, nil
}

func (t *Tool) ID() string {
	return ToolName
}

func (t *Tool) GetParams(req *openaiapi.Request, endpoint openaiapi.Endpoint) (*openaiapi.ToolParams, openaiapi.ToolSession, error) {
	switch endpoint {
	case openaiapi.EndpointChatCompletions:
		return t.getChatParams(req)
	case openaiapi.EndpointResponses:
		return t.getResponsesParams(req)
	default:
		return nil, nil, nil
	}
}

func (p *toolSession) Execute(ctx context.Context, call *openaiapi.ToolCall) (*openaiapi.ExecutionResult, error) {
	if call == nil {
		return nil, fmt.Errorf("tool call is required")
	}
	runtime, err := p.runtimeForExecution()
	if err != nil {
		return nil, err
	}

	switch call.Endpoint {
	case openaiapi.EndpointChatCompletions:
		return executeChatCall(ctx, runtime, call)
	case openaiapi.EndpointResponses:
		return executeResponsesCall(ctx, runtime, p.includeOutputs, call)
	default:
		return nil, fmt.Errorf("unsupported endpoint %q", call.Endpoint)
	}
}

func (p *toolSession) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runtime == nil {
		return nil
	}
	return p.runtime.Close(ctx)
}

func (p *toolSession) runtimeForExecution() (runtime, error) {
	if p == nil {
		return nil, fmt.Errorf("code interpreter runtime is not prepared")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.runtime != nil {
		return p.runtime, nil
	}
	if p.newRuntime == nil {
		return nil, fmt.Errorf("code interpreter tool is not available")
	}

	runtime, err := p.newRuntime()
	if err != nil {
		return nil, err
	}
	p.runtime = runtime
	return p.runtime, nil
}

func (r *sandboxRuntime) Execute(ctx context.Context, callID, rawArgs string) (Result, error) {
	return r.manager.Execute(ctx, callID, rawArgs)
}

func (r *sandboxRuntime) Close(ctx context.Context) error {
	if r == nil || r.manager == nil {
		return nil
	}
	return r.manager.Close(ctx)
}

func (t *Tool) getChatParams(req *openaiapi.Request) (*openaiapi.ToolParams, openaiapi.ToolSession, error) {
	if req == nil || req.Chat == nil || !hasNonNullRawMessage(req.Chat.CodeInterpreterOptions) {
		return nil, nil, nil
	}

	config, err := parseContainerConfigRaw(req.Chat.CodeInterpreterOptions, true)
	if err != nil {
		return nil, nil, err
	}
	session, err := NewSession(config)
	if err != nil {
		return nil, nil, err
	}
	toolBytes, err := json.Marshal(chatCodeInterpreterToolParam())
	if err != nil {
		return nil, nil, err
	}

	return &openaiapi.ToolParams{
		ChatTools:   []openaiapi.ChatToolSpec{toolBytes},
		CallNames:   []string{ToolName},
		StripFields: []string{"code_interpreter_options"},
	}, newToolSession(t, req.AuthToken, session, false), nil
}

func (t *Tool) getResponsesParams(req *openaiapi.Request) (*openaiapi.ToolParams, openaiapi.ToolSession, error) {
	if req == nil || req.Responses == nil {
		return nil, nil, nil
	}

	toolsRaw, err := rawArray(req.RawFields["tools"])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid parameter: 'tools' must be an array")
	}

	hasBuiltinCI := false
	for _, tool := range req.Responses.Params.Tools {
		if tool.OfCodeInterpreter != nil {
			hasBuiltinCI = true
		}
	}
	if !hasBuiltinCI {
		return nil, nil, nil
	}

	var (
		config         ContainerConfig
		foundCI        bool
		includeOutputs bool
	)

	for idx, tool := range req.Responses.Params.Tools {
		if tool.OfCodeInterpreter == nil {
			continue
		}

		if foundCI {
			return nil, nil, fmt.Errorf("only one code_interpreter tool is supported in this increment")
		}
		foundCI = true

		var toolRaw json.RawMessage
		if idx < len(toolsRaw) {
			toolRaw = toolsRaw[idx]
		}
		parsed, err := parseResponsesContainerConfigRaw(toolRaw)
		if err != nil {
			return nil, nil, err
		}
		config = parsed
	}

	for _, include := range req.Responses.Params.Include {
		if string(include) == "code_interpreter_call.outputs" {
			includeOutputs = true
			break
		}
	}

	session, err := NewSession(config)
	if err != nil {
		return nil, nil, err
	}

	injectedTool, err := json.Marshal(responsesCodeInterpreterToolParam())
	if err != nil {
		return nil, nil, err
	}

	return &openaiapi.ToolParams{
		ResponseTools:          []openaiapi.ResponseToolSpec{injectedTool},
		CallNames:              []string{ToolName},
		ResponseInterceptTypes: []string{ToolName},
	}, newToolSession(t, req.AuthToken, session, includeOutputs), nil
}

func (t *Tool) newRuntime(callerAPIKey string, session *Session) (runtime, error) {
	if session == nil {
		return nil, fmt.Errorf("code interpreter session is required")
	}
	if t.controlPlaneURL != "" {
		if !session.Managed {
			return nil, fmt.Errorf("explicit code interpreter container selection is not supported")
		}
		return &sandboxRuntime{
			manager: NewSandboxManager(
				t.sandboxSpec,
				session,
				NewSandboxControlplaneClient(t.controlPlaneURL, callerAPIKey),
				t.sandboxBootstrapper,
			),
		}, nil
	}
	return nil, fmt.Errorf("code interpreter is not configured")
}

func executeChatCall(ctx context.Context, runtime runtime, call *openaiapi.ToolCall) (*openaiapi.ExecutionResult, error) {
	toolCall, err := toolCallObject(call)
	if err != nil {
		return nil, err
	}
	function := rawJSONMap(toolCall["function"])
	result, err := runtime.Execute(ctx, jsonString(toolCall["id"]), jsonString(function["arguments"]))
	if err != nil {
		return nil, err
	}
	return &openaiapi.ExecutionResult{
		ChatPatch: map[string]any{
			"status":       result.Status,
			"container_id": result.ContainerID,
			"outputs":      outputsToAny(result.Outputs),
		},
	}, nil
}

func executeResponsesCall(ctx context.Context, runtime runtime, includeOutputs bool, call *openaiapi.ToolCall) (*openaiapi.ExecutionResult, error) {
	item, err := toolCallObject(call)
	if err != nil {
		return nil, err
	}
	result, err := runtime.Execute(ctx, jsonString(item["call_id"]), jsonString(item["arguments"]))
	if err != nil {
		return nil, err
	}

	publicItem := map[string]any{
		"id":           firstNonEmpty(jsonString(item["id"]), result.ID),
		"type":         "code_interpreter_call",
		"code":         result.Code,
		"container_id": result.ContainerID,
		"status":       result.Status,
	}
	if includeOutputs {
		publicItem["outputs"] = outputsToAny(result.Outputs)
	}

	toolOutputPayload := map[string]any{
		"status":       result.Status,
		"container_id": result.ContainerID,
		"outputs":      outputsToAny(result.Outputs),
		"exit_code":    result.ExitCode,
	}

	return &openaiapi.ExecutionResult{
		ResponsesPublicItem: publicItem,
		ResponsesReplayItem: map[string]any{
			"type":    "function_call_output",
			"call_id": jsonString(item["call_id"]),
			"output":  compactJSONString(toolOutputPayload),
		},
	}, nil
}

func normalizeResponsesToolChoice(req *openaiapi.Request, fields map[string]json.RawMessage, hasOtherTools bool) error {
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
		if err := json.Unmarshal(rawChoice, &mode); err == nil && strings.TrimSpace(mode) == "required" {
			fields["tool_choice"] = json.RawMessage(`"auto"`)
		}
		return nil
	}

	choice := req.Responses.Params.ToolChoice
	choiceType := strings.TrimSpace(stringValue(choice.GetType()))
	choiceName := strings.TrimSpace(stringValue(choice.GetName()))

	if (choiceType == ToolName || (choiceType == "function" && choiceName == ToolName)) && !hasOtherTools {
		// The hosted "code_interpreter" tool_choice is rewritten to the plain
		// "auto" string because some Responses API backends (e.g. vLLM Harmony)
		// do not support "required". The rewritten function tool will still be
		// called by the model given an appropriate prompt.
		fields["tool_choice"] = json.RawMessage(`"auto"`)
		return nil
	}

	// When there are other user tools present and the tool_choice targets one
	// of those tools (not code_interpreter), rewrite to "auto". Object-form
	// tool_choice (e.g. {type:"function", name:"..."}) is not supported by some
	// Responses API backends (e.g. vLLM Harmony); "auto" is accepted universally
	// and the model will still call the indicated tool given an appropriate prompt.
	if hasOtherTools && choiceType != ToolName && !(choiceType == "function" && choiceName == ToolName) {
		fields["tool_choice"] = json.RawMessage(`"auto"`)
		return nil
	}

	return fmt.Errorf("unsupported responses tool_choice for intercepted code_interpreter request")
}

func parseContainerConfig(value any, defaultAuto bool) (ContainerConfig, error) {
	switch typed := value.(type) {
	case nil:
		if defaultAuto {
			return ContainerConfig{Auto: &AutoConfig{}}, nil
		}
		return ContainerConfig{}, fmt.Errorf("code interpreter container config is required")
	case string:
		if typed == "" {
			if defaultAuto {
				return ContainerConfig{Auto: &AutoConfig{}}, nil
			}
			return ContainerConfig{}, fmt.Errorf("code interpreter container config is required")
		}
		return ContainerConfig{ContainerID: typed}, nil
	case map[string]any:
		if len(typed) == 0 {
			return ContainerConfig{Auto: &AutoConfig{}}, nil
		}

		autoType := jsonString(typed["type"])
		if autoType == "" {
			autoType = "auto"
		}
		if autoType != "auto" {
			return ContainerConfig{}, fmt.Errorf("unsupported code interpreter container type %q", autoType)
		}
		if len(rawJSONArray(typed["file_ids"])) > 0 {
			return ContainerConfig{}, fmt.Errorf("code interpreter file_ids are not implemented in this increment")
		}
		if networkPolicy := rawJSONMap(typed["network_policy"]); networkPolicy != nil {
			if jsonString(networkPolicy["type"]) != "disabled" {
				return ContainerConfig{}, fmt.Errorf("only code interpreter network_policy type \"disabled\" is supported in this increment")
			}
		}
		if _, ok := typed["memory_limit"]; ok {
			return ContainerConfig{}, fmt.Errorf("code interpreter memory_limit is not supported in this increment")
		}
		if _, ok := typed["cpus"]; ok {
			return ContainerConfig{}, fmt.Errorf("code interpreter cpus is not supported in this increment")
		}

		return ContainerConfig{
			Auto: &AutoConfig{
				TTLSeconds: jsonInt32(typed["ttl"]),
			},
		}, nil
	default:
		return ContainerConfig{}, fmt.Errorf("invalid code interpreter container config")
	}
}

func toolCallObject(call *openaiapi.ToolCall) (map[string]any, error) {
	if call == nil {
		return nil, fmt.Errorf("tool call is required")
	}
	if call.Value != nil {
		return call.Value, nil
	}
	return rawObject(call.Raw)
}

func parseContainerConfigRaw(raw json.RawMessage, defaultAuto bool) (ContainerConfig, error) {
	if len(raw) == 0 {
		return parseContainerConfig(nil, defaultAuto)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ContainerConfig{}, fmt.Errorf("invalid code interpreter container config")
	}
	return parseContainerConfig(value, defaultAuto)
}

func parseResponsesContainerConfigRaw(toolRaw json.RawMessage) (ContainerConfig, error) {
	if len(toolRaw) == 0 {
		return parseContainerConfig(nil, true)
	}
	var toolFields map[string]json.RawMessage
	if err := json.Unmarshal(toolRaw, &toolFields); err != nil {
		return ContainerConfig{}, fmt.Errorf("invalid code interpreter tool")
	}
	containerRaw := toolFields["container"]
	if len(containerRaw) == 0 {
		containerRaw = json.RawMessage(`{"type":"auto"}`)
	}
	return parseContainerConfigRaw(containerRaw, true)
}

func chatToolHasName(tool openai.ChatCompletionToolUnionParam, name string) bool {
	return tool.OfFunction != nil && strings.TrimSpace(tool.OfFunction.Function.Name) == name
}

func responsesToolHasName(tool responses.ToolUnionParam, name string) bool {
	if tool.OfCodeInterpreter != nil {
		return name == ToolName
	}
	return tool.OfFunction != nil && strings.TrimSpace(tool.OfFunction.Name) == name
}

func chatCodeInterpreterToolParam() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionToolUnionParam{
		OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        ToolName,
				Description: openai.String(toolDescription),
				Strict:      openai.Bool(true),
				Parameters:  codeInterpreterParameters(),
			},
		},
	}
}

func responsesCodeInterpreterToolParam() responses.ToolUnionParam {
	return responses.ToolUnionParam{
		OfFunction: &responses.FunctionToolParam{
			Name:        ToolName,
			Description: openai.String(toolDescription),
			Strict:      openai.Bool(true),
			Parameters:  codeInterpreterParameters(),
		},
	}
}

func codeInterpreterParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "Python code to execute inside the reusable sandbox container.",
			},
			"container_id": map[string]any{
				"type":        "string",
				"description": "Optional container ID to reuse an existing sandbox session.",
			},
		},
		"required":             []string{"code"},
		"additionalProperties": false,
	}
}

func outputsToAny(outputs []Output) []any {
	if len(outputs) == 0 {
		return nil
	}
	result := make([]any, 0, len(outputs))
	for _, output := range outputs {
		item := map[string]any{"type": output.Type}
		if output.Logs != "" {
			item["logs"] = output.Logs
		}
		if output.URL != "" {
			item["url"] = output.URL
		}
		result = append(result, item)
	}
	return result
}

func rawObject(raw json.RawMessage) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func cloneRawFields(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, value := range src {
		dst[key] = cloneRawMessage(value)
	}
	return dst
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func hasNonNullRawMessage(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func marshalRawFields(fields map[string]json.RawMessage) ([]byte, error) {
	if fields == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(fields)
}

func rawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func rawJSONMap(value any) map[string]any {
	existing, _ := value.(map[string]any)
	return existing
}

func rawJSONArray(value any) []any {
	existing, _ := value.([]any)
	return existing
}

func jsonString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func jsonInt32(value any) int32 {
	switch typed := value.(type) {
	case float64:
		return int32(typed)
	case float32:
		return int32(typed)
	case int:
		return int32(typed)
	case int32:
		return typed
	case int64:
		return int32(typed)
	default:
		return 0
	}
}

func compactJSONString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	payload, _ := json.Marshal(value)
	return string(payload)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
