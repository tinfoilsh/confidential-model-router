package openaiapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

type Runner struct {
	tools []Tool
}

func NewRunner(tools ...Tool) *Runner {
	cloned := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if tool != nil {
			cloned = append(cloned, tool)
		}
	}
	return &Runner{tools: cloned}
}

func (r *Runner) Prepare(req *Request) (*ActiveTool, error) {
	if r == nil || req == nil {
		return nil, nil
	}

	var active *ActiveTool
	for _, tool := range r.tools {
		prepared, err := tool.Prepare(req)
		if err != nil {
			return nil, err
		}
		if prepared == nil {
			continue
		}
		if active != nil {
			return nil, fmt.Errorf("requests that mix built-in %s and %s are not supported", active.Tool.ID(), tool.ID())
		}
		active = &ActiveTool{
			Tool:     tool,
			Prepared: prepared,
		}
	}

	return active, nil
}

func (r *Runner) HandleJSON(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, active *ActiveTool, invoker *manager.UpstreamInvoker) error {
	if active == nil || active.Prepared == nil || active.Tool == nil || invoker == nil {
		return nil
	}

	if _, ok := active.Executor(); !ok {
		resetRequestBody(httpReq, active.Prepared.Body)
		invoker.Enclave().ServeHTTP(w, httpReq)
		return nil
	}

	switch req.Kind {
	case EndpointChatCompletions:
		return r.handleChatJSON(ctx, w, httpReq, req, active, invoker)
	case EndpointResponses:
		return r.handleResponsesJSON(ctx, w, httpReq, req, active, invoker)
	default:
		return nil
	}
}

func (r *Runner) HandleStream(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, active *ActiveTool, invoker *manager.UpstreamInvoker) error {
	if active == nil || active.Prepared == nil || active.Tool == nil || invoker == nil {
		return nil
	}

	if _, ok := active.Executor(); !ok {
		resetRequestBody(httpReq, active.Prepared.Body)
		invoker.Enclave().ServeHTTP(w, httpReq)
		return nil
	}

	switch req.Kind {
	case EndpointChatCompletions:
		return r.handleChatStream(ctx, w, httpReq, req, active, invoker)
	case EndpointResponses:
		return r.handleResponsesStream(ctx, w, httpReq, req, active, invoker)
	default:
		return nil
	}
}

func closePreparedState(ctx context.Context, prepared *PreparedRequest) error {
	if prepared == nil || prepared.State == nil {
		return nil
	}
	return prepared.State.Close(ctx)
}
