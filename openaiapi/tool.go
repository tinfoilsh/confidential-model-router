package openaiapi

import (
	"context"
	"encoding/json"
)

type Tool interface {
	ID() string
	Prepare(req *Request) (*PreparedRequest, error)
}

type ExecutableTool interface {
	Tool
	Execute(ctx context.Context, call *InferenceToolCall) (*ExecutionResult, error)
}

type PreparedRequest struct {
	EffectiveModel string
	Body           []byte
	State          any
}

type InferenceToolCall struct {
	Endpoint Endpoint
	Raw      json.RawMessage
	State    any
}

type ExecutionResult struct {
	ChatPatch           map[string]any
	ResponsesPublicItem map[string]any
	ResponsesReplayItem map[string]any
}

type StateCloser interface {
	Close(context.Context) error
}

type ActiveTool struct {
	Tool     Tool
	Prepared *PreparedRequest
}

func (a *ActiveTool) Executable() (ExecutableTool, bool) {
	if a == nil || a.Tool == nil {
		return nil, false
	}
	tool, ok := a.Tool.(ExecutableTool)
	return tool, ok
}
