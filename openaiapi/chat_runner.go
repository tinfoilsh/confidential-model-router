package openaiapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

type chatToolCallBuilder struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

type chatChunkMetadata struct {
	ID                string
	Created           int64
	Model             string
	ServiceTier       string
	SystemFingerprint string
}

func (r *Runner) handleChatJSON(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, active *ActiveTool, invoker *manager.UpstreamInvoker) error {
	execTool, _ := active.Executable()
	cleanupCtx, cancelCleanup := cleanupContext(httpReq)
	defer cancelCleanup()
	defer func() {
		_ = closePreparedState(cleanupCtx, active.Prepared)
	}()

	resp, err := invoker.Do(ctx, httpReq.Method, httpReq.URL.Path, httpReq.Header, active.Prepared.Body)
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}
	w.Header().Set("Tinfoil-Enclave", invoker.Enclave().String())

	responseBody, err := readResponseBody(resp)
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}

	usage := extractUsageFromBody(responseBody)
	invoker.RecordUsage(httpReq, requestIDFromResponse(resp), usage, false)

	if resp.StatusCode >= http.StatusBadRequest {
		forwardResponse(w, resp, responseBody)
		return nil
	}

	var completion openai.ChatCompletion
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}
	if !chatCompletionHasToolCalls(&completion, active.Tool.ID()) {
		setUsageHeaderOrTrailer(w, httpReq, usage)
		forwardResponse(w, resp, responseBody)
		return nil
	}

	payload, err := copyResponseMap(responseBody)
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}

	for _, choiceValue := range rawJSONArray(payload["choices"]) {
		choice := rawJSONMap(choiceValue)
		message := rawJSONMap(choice["message"])
		toolCalls := rawJSONArray(message["tool_calls"])
		for _, toolCallValue := range toolCalls {
			toolCall := rawJSONMap(toolCallValue)
			function := rawJSONMap(toolCall["function"])
			if jsonString(function["name"]) != active.Tool.ID() {
				continue
			}

			raw, err := json.Marshal(toolCall)
			if err != nil {
				return err
			}
			result, execErr := execTool.Execute(ctx, &InferenceToolCall{
				Endpoint: EndpointChatCompletions,
				Raw:      raw,
				State:    active.Prepared.State,
			})
			if execErr != nil {
				writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
				return nil
			}
			mergeMap(toolCall, result.ChatPatch)
		}
	}

	setUsageHeaderOrTrailer(w, httpReq, usage)
	return writeJSON(w, resp.StatusCode, payload)
}

func (r *Runner) handleChatStream(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, active *ActiveTool, invoker *manager.UpstreamInvoker) error {
	cleanupCtx, cancelCleanup := cleanupContext(httpReq)
	defer cancelCleanup()
	defer func() {
		_ = closePreparedState(cleanupCtx, active.Prepared)
	}()

	resp, err := invoker.Do(ctx, httpReq.Method, httpReq.URL.Path, httpReq.Header, active.Prepared.Body)
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return nil
	}
	w.Header().Set("Tinfoil-Enclave", invoker.Enclave().String())

	if resp.StatusCode >= http.StatusBadRequest {
		responseBody, err := readResponseBody(resp)
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
			return nil
		}
		forwardResponse(w, resp, responseBody)
		return nil
	}

	return r.streamChatResponse(w, httpReq, resp, invoker, active)
}

func (r *Runner) streamChatResponse(w http.ResponseWriter, httpReq *http.Request, resp *http.Response, invoker *manager.UpstreamInvoker, active *ActiveTool) error {
	execTool, _ := active.Executable()
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	prepareUsageTrailer(w, httpReq)

	flusher, _ := w.(http.Flusher)
	requestID := requestIDFromResponse(resp)

	builders := map[int]map[int]*chatToolCallBuilder{}
	var lastChunk *chatChunkMetadata
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

		var chunk openai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			_, _ = io.WriteString(w, line+"\n")
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}

		if usage := usageFromChatChunk(chunk); usage != nil {
			finalUsage = usage
		}
		if isUsageOnlyTypedChatChunk(chunk) && !clientRequestedStreamingUsage(httpReq) {
			suppressNextBlank = true
			continue
		}

		lastChunk = chunkMetadataFromChunk(chunk)
		collectChatToolCalls(builders, chunk)

		_, _ = io.WriteString(w, line+"\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	invoker.RecordUsage(httpReq, requestID, finalUsage, true)

	hasExecution := false
	for choiceIndex, choiceBuilders := range builders {
		for toolIndex, builder := range choiceBuilders {
			if builder == nil || builder.Name != active.Tool.ID() {
				continue
			}

			raw, err := json.Marshal(map[string]any{
				"id": builder.ID,
				"function": map[string]any{
					"name":      builder.Name,
					"arguments": builder.Arguments.String(),
				},
			})
			if err != nil {
				return err
			}
			result, err := execTool.Execute(httpReq.Context(), &InferenceToolCall{
				Endpoint: EndpointChatCompletions,
				Raw:      raw,
				State:    active.Prepared.State,
			})
			if err != nil {
				return err
			}

			hasExecution = true
			chunk := buildChatExecutionChunk(lastChunk, choiceIndex, toolIndex, builder.ID, result.ChatPatch)
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

	setUsageHeaderOrTrailer(w, httpReq, finalUsage)

	if hasExecution || lastChunk != nil {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	return nil
}

func cleanupContext(httpReq *http.Request) (context.Context, context.CancelFunc) {
	base := context.Background()
	if httpReq != nil {
		base = context.WithoutCancel(httpReq.Context())
	}
	return context.WithTimeout(base, 30*time.Second)
}

func collectChatToolCalls(builders map[int]map[int]*chatToolCallBuilder, chunk openai.ChatCompletionChunk) {
	for _, choice := range chunk.Choices {
		if len(choice.Delta.ToolCalls) == 0 {
			continue
		}
		choiceIndex := int(choice.Index)
		if builders[choiceIndex] == nil {
			builders[choiceIndex] = map[int]*chatToolCallBuilder{}
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			toolIndex := int(toolCall.Index)
			builder := builders[choiceIndex][toolIndex]
			if builder == nil {
				builder = &chatToolCallBuilder{}
				builders[choiceIndex][toolIndex] = builder
			}
			if id := strings.TrimSpace(toolCall.ID); id != "" {
				builder.ID = id
			}
			if name := strings.TrimSpace(toolCall.Function.Name); name != "" {
				builder.Name = name
			}
			if toolCall.Function.Arguments != "" {
				builder.Arguments.WriteString(toolCall.Function.Arguments)
			}
		}
	}
}

func buildChatExecutionChunk(lastChunk *chatChunkMetadata, choiceIndex int, toolIndex int, toolCallID string, patch map[string]any) map[string]any {
	toolCall := map[string]any{
		"index": toolIndex,
	}
	if strings.TrimSpace(toolCallID) != "" {
		toolCall["id"] = toolCallID
	}
	mergeMap(toolCall, patch)

	chunk := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{
			map[string]any{
				"index": choiceIndex,
				"delta": map[string]any{
					"tool_calls": []any{toolCall},
				},
			},
		},
	}
	if lastChunk == nil {
		return chunk
	}
	if lastChunk.ID != "" {
		chunk["id"] = lastChunk.ID
	}
	if lastChunk.Created != 0 {
		chunk["created"] = lastChunk.Created
	}
	if lastChunk.Model != "" {
		chunk["model"] = lastChunk.Model
	}
	if lastChunk.ServiceTier != "" {
		chunk["service_tier"] = lastChunk.ServiceTier
	}
	if lastChunk.SystemFingerprint != "" {
		chunk["system_fingerprint"] = lastChunk.SystemFingerprint
	}
	return chunk
}

func chatCompletionHasToolCalls(completion *openai.ChatCompletion, toolName string) bool {
	if completion == nil {
		return false
	}
	for _, choice := range completion.Choices {
		for _, toolCall := range choice.Message.ToolCalls {
			if functionCall, ok := toolCall.AsAny().(openai.ChatCompletionMessageFunctionToolCall); ok {
				if strings.TrimSpace(functionCall.Function.Name) == toolName {
					return true
				}
			}
		}
	}
	return false
}

func chunkMetadataFromChunk(chunk openai.ChatCompletionChunk) *chatChunkMetadata {
	return &chatChunkMetadata{
		ID:                chunk.ID,
		Created:           chunk.Created,
		Model:             chunk.Model,
		ServiceTier:       string(chunk.ServiceTier),
		SystemFingerprint: chunk.SystemFingerprint,
	}
}

func isUsageOnlyTypedChatChunk(chunk openai.ChatCompletionChunk) bool {
	return chunk.JSON.Usage.Valid() && len(chunk.Choices) == 0
}

func usageFromChatChunk(chunk openai.ChatCompletionChunk) *tokencount.Usage {
	if !chunk.JSON.Usage.Valid() {
		return nil
	}
	raw, err := json.Marshal(chunk.Usage)
	if err != nil {
		return nil
	}
	var usage tokencount.Usage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil
	}
	usage.Normalize()
	return &usage
}

func ensureExecutionPatch(result *ExecutionResult) error {
	if result == nil {
		return fmt.Errorf("missing execution result")
	}
	return nil
}
