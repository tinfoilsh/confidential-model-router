package codeinterpreter

import (
	"encoding/json"
	"fmt"
	"strings"
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
	execTimeout         time.Duration
	sandboxBootstrapper sandboxBootstrapper
	sandboxSpec         SandboxSpec
}

func New(cfg Config) (*Tool, error) {
	tool := &Tool{}

	if strings.TrimSpace(cfg.Repo) != "" {
		if strings.TrimSpace(cfg.ControlPlaneURL) == "" {
			return nil, fmt.Errorf("control plane url is required when managed sandboxes are enabled")
		}
		tool.controlPlaneURL = cfg.ControlPlaneURL
		tool.execTimeout = cfg.ExecTimeout
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

func (t *Tool) getChatParams(req *openaiapi.Request) (*openaiapi.ToolParams, openaiapi.ToolSession, error) {
	if req == nil || req.Chat == nil || !hasNonNullRawMessage(req.Chat.CodeInterpreterOptions) {
		return nil, nil, nil
	}

	config, err := parseChatContainerConfig(req.Chat.CodeInterpreterOptions)
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
	}, newToolSession(t, req.AuthToken, config, false), nil
}

func (t *Tool) getResponsesParams(req *openaiapi.Request) (*openaiapi.ToolParams, openaiapi.ToolSession, error) {
	if req == nil || req.Responses == nil {
		return nil, nil, nil
	}

	toolsRaw, err := rawArray(req.RawFields["tools"])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid parameter: 'tools' must be an array")
	}

	config, foundCI, err := parseResponsesContainerConfig(toolsRaw)
	if err != nil {
		return nil, nil, err
	}
	if !foundCI {
		return nil, nil, nil
	}

	includeOutputs := false
	for _, include := range req.Responses.Params.Include {
		if string(include) == "code_interpreter_call.outputs" {
			includeOutputs = true
			break
		}
	}

	injectedTool, err := json.Marshal(responsesCodeInterpreterToolParam())
	if err != nil {
		return nil, nil, err
	}

	return &openaiapi.ToolParams{
		ResponseTools:          []openaiapi.ResponseToolSpec{injectedTool},
		CallNames:              []string{ToolName},
		ResponseInterceptTypes: []string{ToolName},
	}, newToolSession(t, req.AuthToken, config, includeOutputs), nil
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

func hasNonNullRawMessage(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}
