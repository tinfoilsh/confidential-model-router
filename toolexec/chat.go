package toolexec

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
	"github.com/tinfoilsh/confidential-model-router/toolexec/codeinterpreter"
)

type chatToolCallBuilder struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

type normalizedChatRequest struct {
	body    map[string]any
	session *codeinterpreter.Session
}

func (e *Executor) handleChat(ctx context.Context, w http.ResponseWriter, r *http.Request, body map[string]any, invoker *manager.UpstreamInvoker) error {
	normalized, err := normalizeChatRequest(body)
	if err != nil {
		return err
	}

	bodyBytes, err := json.Marshal(normalized.body)
	if err != nil {
		return err
	}

	resp, err := invoker.Do(ctx, r.Method, r.URL.Path, r.Header, bodyBytes)
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}
	w.Header().Set("Tinfoil-Enclave", invoker.Enclave().String())

	if normalized.body["stream"] == true {
		if resp.StatusCode >= http.StatusBadRequest {
			responseBody, err := readResponseBody(resp)
			if err != nil {
				writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
				return nil
			}
			forwardResponse(w, resp, responseBody)
			return nil
		}
		return e.handleChatStream(w, r, resp, invoker, normalized.session)
	}

	responseBody, err := readResponseBody(resp)
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}

	usage := extractUsageFromBody(responseBody)
	invoker.RecordUsage(r, requestIDFromResponse(resp), usage, false)

	if resp.StatusCode >= http.StatusBadRequest {
		forwardResponse(w, resp, responseBody)
		return nil
	}

	payload, err := copyResponseMap(responseBody)
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}

	choices := rawJSONArray(payload["choices"])
	for _, choiceValue := range choices {
		choice := rawJSONMap(choiceValue)
		message := rawJSONMap(choice["message"])
		toolCalls := rawJSONArray(message["tool_calls"])
		for _, toolCallValue := range toolCalls {
			toolCall := rawJSONMap(toolCallValue)
			function := rawJSONMap(toolCall["function"])
			if jsonString(function["name"]) != codeInterpreterToolName {
				continue
			}
			log.WithField("arguments", jsonString(function["arguments"])).Info("code_interpreter tool call (non-streaming)")
			result, execErr := e.codeInterpreter.Execute(ctx, jsonString(toolCall["id"]), jsonString(function["arguments"]), normalized.session, bearerToken(r))
			if execErr != nil {
				writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
				return nil
			}
			toolCall["status"] = result.Status
			toolCall["container_id"] = result.ContainerID
			toolCall["outputs"] = outputsToAny(result.Outputs)
		}
	}

	setUsageHeaderOrTrailer(w, r, usage)
	if err := writeJSON(w, resp.StatusCode, payload); err != nil {
		return err
	}
	return nil
}

func normalizeChatRequest(body map[string]any) (*normalizedChatRequest, error) {
	configValue, exists := body["code_interpreter_options"]
	if !exists {
		return nil, fmt.Errorf("code_interpreter_options is required")
	}

	tools := append([]any(nil), rawJSONArray(body["tools"])...)
	for _, toolValue := range tools {
		tool := rawJSONMap(toolValue)
		if toolHasName(tool, codeInterpreterToolName) {
			return nil, fmt.Errorf("tool name collision with reserved tool %q", codeInterpreterToolName)
		}
	}

	config, err := parseContainerConfig(configValue, true)
	if err != nil {
		return nil, err
	}
	session, err := codeinterpreter.NewSession(config)
	if err != nil {
		return nil, err
	}

	normalized, err := deepCopyMap(body)
	if err != nil {
		return nil, err
	}
	delete(normalized, "code_interpreter_options")
	tools = append(tools, toolSchema())
	normalized["tools"] = tools

	return &normalizedChatRequest{
		body:    normalized,
		session: session,
	}, nil
}

func (e *Executor) handleChatStream(w http.ResponseWriter, req *http.Request, resp *http.Response, invoker *manager.UpstreamInvoker, session *codeinterpreter.Session) error {
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	prepareUsageTrailer(w, req)

	flusher, _ := w.(http.Flusher)

	requestID := requestIDFromResponse(resp)

	builders := map[int]map[int]*chatToolCallBuilder{}
	var lastChunk map[string]any
	var finalUsage *tokencount.Usage

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024), 16*1024*1024)
	suppressNextBlank := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if suppressNextBlank {
				suppressNextBlank = false
				continue
			}
			_, _ = io.WriteString(w, "\n")
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			_, _ = io.WriteString(w, line+"\n")
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			_, _ = io.WriteString(w, line+"\n")
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}

		if usage := collectChunkUsage(chunk); usage != nil {
			finalUsage = usage
		}
		if isUsageOnlyChatChunk(chunk) && !clientRequestedStreamingUsage(req) {
			suppressNextBlank = true
			continue
		}

		lastChunk = chunk
		collectChatToolCalls(builders, chunk)

		_, _ = io.WriteString(w, line+"\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	invoker.RecordUsage(req, requestID, finalUsage, true)

	hasExecution := false
	for choiceIndex, choiceBuilders := range builders {
		for toolIndex, builder := range choiceBuilders {
			if builder == nil || builder.Name != codeInterpreterToolName {
				continue
			}
			log.WithField("arguments", builder.Arguments.String()).Info("code_interpreter tool call (streaming)")
			result, err := e.codeInterpreter.Execute(req.Context(), builder.ID, builder.Arguments.String(), session, bearerToken(req))
			if err != nil {
				return err
			}
			hasExecution = true
			chunk := buildChatExecutionChunk(lastChunk, choiceIndex, toolIndex, builder.ID, result)
			payload, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			if _, err := w.Write([]byte("data: " + string(payload) + "\n\n")); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}

	setUsageHeaderOrTrailer(w, req, finalUsage)

	if hasExecution || lastChunk != nil {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	return nil
}

func collectChatToolCalls(builders map[int]map[int]*chatToolCallBuilder, chunk map[string]any) {
	choices := rawJSONArray(chunk["choices"])
	for _, choiceValue := range choices {
		choice := rawJSONMap(choiceValue)
		index, _ := choice["index"].(float64)
		delta := rawJSONMap(choice["delta"])
		toolCalls := rawJSONArray(delta["tool_calls"])
		if len(toolCalls) == 0 {
			continue
		}
		choiceIndex := int(index)
		if builders[choiceIndex] == nil {
			builders[choiceIndex] = map[int]*chatToolCallBuilder{}
		}
		for _, toolCallValue := range toolCalls {
			toolCall := rawJSONMap(toolCallValue)
			toolIndexFloat, _ := toolCall["index"].(float64)
			toolIndex := int(toolIndexFloat)
			builder := builders[choiceIndex][toolIndex]
			if builder == nil {
				builder = &chatToolCallBuilder{}
				builders[choiceIndex][toolIndex] = builder
			}
			if id := jsonString(toolCall["id"]); id != "" {
				builder.ID = id
			}
			function := rawJSONMap(toolCall["function"])
			if name := jsonString(function["name"]); name != "" {
				builder.Name = name
			}
			if args, ok := function["arguments"].(string); ok {
				builder.Arguments.WriteString(args)
			}
		}
	}
}

func outputsToAny(outputs []codeinterpreter.Output) []any {
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

func buildChatExecutionChunk(lastChunk map[string]any, choiceIndex int, toolIndex int, toolCallID string, result codeinterpreter.Result) map[string]any {
	chunk := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{
			map[string]any{
				"index": choiceIndex,
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{
							"index":        toolIndex,
							"id":           toolCallID,
							"status":       result.Status,
							"container_id": result.ContainerID,
							"outputs":      outputsToAny(result.Outputs),
						},
					},
				},
			},
		},
	}
	for _, key := range []string{"id", "created", "model", "service_tier", "system_fingerprint"} {
		if value, ok := lastChunk[key]; ok {
			chunk[key] = value
		}
	}
	return chunk
}
