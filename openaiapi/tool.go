package openaiapi

import (
	"context"
	"encoding/json"
)

type Tool interface {
	ID() string
	Prepare(req *Request) (*PreparedRequest, error)
}

type PreparedExecutor interface {
	Execute(ctx context.Context, call *InferenceToolCall) (*ExecutionResult, error)
}

type PreparedRequest struct {
	EffectiveModel string
	Body           []byte
	State          StateCloser
	Executor       PreparedExecutor
}

type InferenceToolCall struct {
	Endpoint Endpoint
	Raw      json.RawMessage
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

func (a *ActiveTool) Executor() (PreparedExecutor, bool) {
	if a == nil || a.Prepared == nil || a.Prepared.Executor == nil {
		return nil, false
	}
	return a.Prepared.Executor, true
}
