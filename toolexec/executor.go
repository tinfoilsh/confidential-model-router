package toolexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// normalizeWebsearchResponsesBody returns a copy of the responses body with
// web_search entries removed from tools. The websearch enclave is purpose-built
// for search and does not need the tool declaration; passing it causes vLLM
// validation errors because "web_search" is not a valid vLLM tool schema type.
func normalizeWebsearchResponsesBody(body map[string]any) (map[string]any, error) {
	normalized, err := deepCopyMap(body)
	if err != nil {
		return nil, err
	}
	var filtered []any
	for _, tool := range rawJSONArray(normalized["tools"]) {
		if jsonString(rawJSONMap(tool)["type"]) != "web_search" {
			filtered = append(filtered, tool)
		}
	}
	if len(filtered) == 0 {
		delete(normalized, "tools")
	} else {
		normalized["tools"] = filtered
	}
	return normalized, nil
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

	// Websearch: normalize the body before forwarding to the websearch enclave.
	// For /v1/responses, the client signals websearch via tools=[{type:"web_search"}],
	// but the websearch enclave does not accept that as a vLLM tool schema. Strip
	// web_search entries from tools so the enclave receives a clean request.
	if isWebsearch(r.URL.Path, body) {
		if r.URL.Path == "/v1/responses" {
			normalized, err := normalizeWebsearchResponsesBody(body)
			if err != nil {
				writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
				return true
			}
			bodyBytes, err := json.Marshal(normalized)
			if err != nil {
				writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
				return true
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			r.ContentLength = int64(len(bodyBytes))
			r.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
		}
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
