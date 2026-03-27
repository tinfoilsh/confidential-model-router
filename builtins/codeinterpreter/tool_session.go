package codeinterpreter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/tinfoilsh/confidential-model-router/openaiapi"
)

type toolSession struct {
	manager        *SandboxManager
	initErr        error // non-nil if manager couldn't be created (e.g., unsupported config)
	includeOutputs bool
	mu             sync.Mutex
	sandbox        *Sandbox
}

func newToolSession(tool *Tool, callerAPIKey string, config ContainerConfig, includeOutputs bool) *toolSession {
	if tool.controlPlaneURL == "" {
		return &toolSession{includeOutputs: includeOutputs}
	}
	if config.ContainerID != "" {
		return &toolSession{
			initErr:        fmt.Errorf("explicit code interpreter container selection is not supported"),
			includeOutputs: includeOutputs,
		}
	}

	ttl := int32(0)
	if config.Auto != nil {
		ttl = config.Auto.TTLSeconds
	}
	spec := tool.sandboxSpec
	spec.TTLSeconds = ttl

	return &toolSession{
		manager:        NewSandboxManager(spec, tool.execTimeout, NewSandboxControlplaneClient(tool.controlPlaneURL, callerAPIKey), tool.sandboxBootstrapper),
		includeOutputs: includeOutputs,
	}
}

func (p *toolSession) getSandbox(ctx context.Context) (*Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.sandbox != nil {
		return p.sandbox, nil
	}
	if p.initErr != nil {
		return nil, p.initErr
	}
	if p.manager == nil {
		return nil, fmt.Errorf("code interpreter is not configured")
	}

	sandbox, err := p.manager.GetSandbox(ctx)
	if err != nil {
		return nil, err
	}
	p.sandbox = sandbox
	return p.sandbox, nil
}

func (p *toolSession) Execute(ctx context.Context, call *openaiapi.ToolCall) (*openaiapi.ExecutionResult, error) {
	if call == nil {
		return nil, fmt.Errorf("tool call is required")
	}

	sandbox, sandboxErr := p.getSandbox(ctx)

	switch call.Endpoint {
	case openaiapi.EndpointChatCompletions:
		return executeChatCall(ctx, sandbox, sandboxErr, call)
	case openaiapi.EndpointResponses:
		return executeResponsesCall(ctx, sandbox, sandboxErr, p.includeOutputs, call)
	default:
		return nil, fmt.Errorf("unsupported endpoint %q", call.Endpoint)
	}
}

func (p *toolSession) Close(ctx context.Context) error {
	if p == nil || p.manager == nil {
		return nil
	}
	return p.manager.Close(ctx)
}

func executeChatCall(ctx context.Context, sandbox *Sandbox, sandboxErr error, call *openaiapi.ToolCall) (*openaiapi.ExecutionResult, error) {
	toolCall, err := toolCallObject(call)
	if err != nil {
		return nil, err
	}
	callID := jsonString(toolCall["id"])
	function := rawJSONMap(toolCall["function"])

	var result Result
	if sandboxErr != nil {
		result = failedResult(callID, "", sandboxErr)
	} else {
		args, err := ParseArgs(jsonString(function["arguments"]))
		if err != nil {
			result = failedResult(callID, "", err)
		} else {
			result, err = sandbox.Execute(ctx, callID, args)
			if err != nil {
				return nil, err
			}
		}
	}

	return &openaiapi.ExecutionResult{
		ChatPatch: map[string]any{
			"status":       result.Status,
			"container_id": result.ContainerID,
			"context_id":   result.ContextID,
			"outputs":      outputsToAny(result.Outputs),
		},
	}, nil
}

func executeResponsesCall(ctx context.Context, sandbox *Sandbox, sandboxErr error, includeOutputs bool, call *openaiapi.ToolCall) (*openaiapi.ExecutionResult, error) {
	item, err := toolCallObject(call)
	if err != nil {
		return nil, err
	}
	callID := jsonString(item["call_id"])

	var result Result
	if sandboxErr != nil {
		result = failedResult(callID, "", sandboxErr)
	} else {
		args, err := ParseArgs(jsonString(item["arguments"]))
		if err != nil {
			result = failedResult(callID, "", err)
		} else {
			result, err = sandbox.Execute(ctx, callID, args)
			if err != nil {
				return nil, err
			}
		}
	}

	publicItem := map[string]any{
		"id":           firstNonEmpty(jsonString(item["id"]), result.ID),
		"type":         "code_interpreter_call",
		"code":         result.Code,
		"container_id": result.ContainerID,
		"context_id":   result.ContextID,
		"status":       result.Status,
	}
	if includeOutputs {
		publicItem["outputs"] = outputsToAny(result.Outputs)
	}

	toolOutputPayload := map[string]any{
		"status":       result.Status,
		"container_id": result.ContainerID,
		"context_id":   result.ContextID,
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

func toolCallObject(call *openaiapi.ToolCall) (map[string]any, error) {
	if call == nil {
		return nil, fmt.Errorf("tool call is required")
	}
	if call.Value != nil {
		return call.Value, nil
	}
	return rawObject(call.Raw)
}

func rawObject(raw json.RawMessage) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
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

func jsonString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
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
