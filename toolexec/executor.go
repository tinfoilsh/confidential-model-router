package toolexec

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/toolexec/codeinterpreter"
)

const websearchModel = "websearch"

// Executor handles router-local internal tool execution.
type Executor struct {
	codeInterpreter *codeinterpreter.Client
}

func New(codeInterpreterBaseURL, codeInterpreterRepo string, execTimeout time.Duration) (*Executor, error) {
	if codeInterpreterBaseURL == "" {
		return &Executor{}, nil
	}
	client, err := codeinterpreter.NewClient(codeInterpreterBaseURL, codeInterpreterRepo, execTimeout)
	if err != nil {
		return nil, err
	}
	return &Executor{codeInterpreter: client}, nil
}

// isWebsearch reports whether the request is a web search request.
func isWebsearch(path string, body map[string]any) bool {
	if _, ok := body["web_search_options"]; ok {
		return true
	}
	if path == "/v1/responses" {
		for _, tool := range rawJSONArray(body["tools"]) {
			if jsonString(rawJSONMap(tool)["type"]) == "web_search" {
				return true
			}
		}
	}
	return false
}

// NeedsHandling reports whether the executor should intercept this request.
// The returned model name is the effective routing target — it may differ from
// modelName (e.g. "websearch" when web-search options are present).
func (e *Executor) NeedsHandling(path, modelName string, body map[string]any) (bool, string) {
	if isWebsearch(path, body) {
		return true, websearchModel
	}
	switch path {
	case "/v1/chat/completions":
		if _, exists := body["code_interpreter_options"]; exists {
			return true, modelName
		}
	case "/v1/responses":
		for _, tool := range rawJSONArray(body["tools"]) {
			if jsonString(rawJSONMap(tool)["type"]) == codeInterpreterToolName {
				return true, modelName
			}
		}
	}
	return false, modelName
}

func (e *Executor) HandleWithInvoker(ctx context.Context, w http.ResponseWriter, r *http.Request, body map[string]any, invoker *manager.UpstreamInvoker) bool {
	if e == nil || invoker == nil {
		return false
	}

	// Websearch: forward the request as-is to the websearch enclave.
	if isWebsearch(r.URL.Path, body) {
		invoker.Enclave().ServeHTTP(w, r)
		return true
	}

	if e.codeInterpreter == nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
		return true
	}

	var err error
	switch r.URL.Path {
	case "/v1/chat/completions":
		err = e.handleChat(ctx, w, r, body, invoker)
	case "/v1/responses":
		err = e.handleResponses(ctx, w, r, body, invoker)
	default:
		return false
	}

	if err == nil {
		return true
	}

	writeAPIError(w, fmt.Sprintf("Invalid request: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
	return true
}
