package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

const (
	chatInputTokensPath      = "/v1/chat/completions/input_tokens"
	responsesInputTokensPath = "/v1/responses/input_tokens"
	tokenizePath             = "/tokenize"
	inputTokensAuthErrorType = "authentication_error"
	maxInputTokensErrorBytes = 1 << 20
)

type inputTokenDispatch func(context.Context, string, string, []byte, http.Header) (*http.Response, error)

type inputTokenModelResolver func(map[string]any) (string, error)

func isInputTokensPath(path string) bool {
	return path == chatInputTokensPath || path == responsesInputTokensPath
}

func handleInputTokens(
	w http.ResponseWriter,
	r *http.Request,
	apiKey string,
	resolveModel inputTokenModelResolver,
	dispatch inputTokenDispatch,
) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		jsonError(w, "Method not allowed.", manager.ErrTypeInvalidRequest, http.StatusMethodNotAllowed)
		return
	}
	if apiKey == "" {
		jsonError(w, "Missing bearer API key.", inputTokensAuthErrorType, http.StatusUnauthorized)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeRequestBodyError(w, err)
		return
	}
	defer r.Body.Close()

	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		jsonError(w, fmt.Sprintf("Invalid request body: %v.", err), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
		return
	}

	modelName, err := inputTokensModel(body, resolveModel)
	if err != nil {
		jsonError(w, err.Error(), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
		return
	}

	var tokenizeBody map[string]any
	switch r.URL.Path {
	case chatInputTokensPath:
		tokenizeBody, err = chatTokenizeBody(body, modelName)
	case responsesInputTokensPath:
		tokenizeBody, err = responsesTokenizeBody(body, modelName)
	default:
		err = fmt.Errorf("unsupported input-token route: %s", r.URL.Path)
	}
	if err != nil {
		jsonError(w, err.Error(), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
		return
	}

	tokenizeBytes, err := json.Marshal(tokenizeBody)
	if err != nil {
		jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
		return
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+apiKey)
	headers.Set("Content-Type", "application/json")
	resp, err := dispatch(r.Context(), modelName, tokenizePath, tokenizeBytes, headers)
	if err != nil {
		jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		forwardInputTokensError(w, resp)
		return
	}

	var tokenized struct {
		Count *int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenized); err != nil {
		jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return
	}
	if tokenized.Count == nil || *tokenized.Count < 0 {
		jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return
	}

	object := "chat.completion.input_tokens"
	if r.URL.Path == responsesInputTokensPath {
		object = "response.input_tokens"
	}
	sendJSON(w, map[string]any{
		"object":       object,
		"input_tokens": *tokenized.Count,
	})
}

func inputTokensModel(body map[string]any, resolveModel inputTokenModelResolver) (string, error) {
	modelValue, ok := body["model"]
	if !ok {
		return "", fmt.Errorf("Missing required parameter: 'model'.")
	}
	modelName, ok := modelValue.(string)
	if !ok || modelName == "" {
		return "", fmt.Errorf("Invalid parameter: 'model' must be a non-empty string.")
	}
	if modelName != "auto" {
		return modelName, nil
	}
	if resolveModel == nil {
		return "", fmt.Errorf("Model 'auto' is not available for this request.")
	}
	return resolveModel(body)
}

func chatTokenizeBody(body map[string]any, modelName string) (map[string]any, error) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("Missing or invalid required parameter: 'messages'.")
	}

	tokenizeBody := map[string]any{
		"model":    modelName,
		"messages": messages,
	}
	if tools, ok := body["tools"]; ok {
		tokenizeBody["tools"] = tools
	}
	return tokenizeBody, nil
}

func responsesTokenizeBody(body map[string]any, modelName string) (map[string]any, error) {
	messages, err := responsesMessages(body)
	if err != nil {
		return nil, err
	}
	tokenizeBody := map[string]any{
		"model":    modelName,
		"messages": messages,
	}
	if tools, ok := body["tools"]; ok {
		converted, err := responsesTools(tools)
		if err != nil {
			return nil, err
		}
		tokenizeBody["tools"] = converted
	}
	return tokenizeBody, nil
}

func responsesMessages(body map[string]any) ([]any, error) {
	messages := make([]any, 0)
	if instructions, ok := body["instructions"]; ok {
		text, ok := instructions.(string)
		if !ok {
			return nil, fmt.Errorf("Invalid parameter: 'instructions' must be a string.")
		}
		if text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	input, ok := body["input"]
	if !ok {
		return nil, fmt.Errorf("Missing required parameter: 'input'.")
	}
	if text, ok := input.(string); ok {
		return append(messages, map[string]any{"role": "user", "content": text}), nil
	}
	items, ok := input.([]any)
	if !ok {
		return nil, fmt.Errorf("Invalid parameter: 'input' must be a string or array.")
	}

	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Invalid item in 'input': expected an object.")
		}
		itemType, _ := item["type"].(string)
		switch itemType {
		case "", "message":
			message, err := responsesMessage(item)
			if err != nil {
				return nil, err
			}
			messages = append(messages, message)
		case "function_call":
			message, err := responsesFunctionCall(item)
			if err != nil {
				return nil, err
			}
			messages = appendFunctionCall(messages, message)
		case "function_call_output":
			message, err := responsesFunctionCallOutput(item)
			if err != nil {
				return nil, err
			}
			messages = append(messages, message)
		default:
			return nil, fmt.Errorf("Unsupported Responses input item type %q.", itemType)
		}
	}
	return messages, nil
}

func responsesMessage(item map[string]any) (map[string]any, error) {
	role, ok := item["role"].(string)
	if !ok || role == "" {
		return nil, fmt.Errorf("Responses message items require a non-empty 'role'.")
	}
	content, ok := item["content"]
	if !ok {
		return nil, fmt.Errorf("Responses message items require 'content'.")
	}
	converted, err := responsesContent(content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"role": role, "content": converted}, nil
}

func responsesContent(content any) (any, error) {
	if text, ok := content.(string); ok {
		return text, nil
	}
	parts, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("Responses message 'content' must be a string or array.")
	}

	converted := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Invalid Responses content part: expected an object.")
		}
		partType, _ := part["type"].(string)
		switch partType {
		case "input_text", "output_text":
			text, ok := part["text"].(string)
			if !ok {
				return nil, fmt.Errorf("Responses %s content requires string 'text'.", partType)
			}
			converted = append(converted, map[string]any{"type": "text", "text": text})
		case "input_image":
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				return nil, fmt.Errorf("Responses input_image content requires string 'image_url'.")
			}
			image := map[string]any{"url": imageURL}
			if detail, ok := part["detail"].(string); ok && detail != "" {
				image["detail"] = detail
			}
			converted = append(converted, map[string]any{"type": "image_url", "image_url": image})
		default:
			return nil, fmt.Errorf("Unsupported Responses content part type %q.", partType)
		}
	}
	return converted, nil
}

func responsesFunctionCall(item map[string]any) (map[string]any, error) {
	callID, callIDOK := item["call_id"].(string)
	name, nameOK := item["name"].(string)
	arguments, argumentsOK := item["arguments"].(string)
	if !callIDOK || callID == "" || !nameOK || name == "" || !argumentsOK {
		return nil, fmt.Errorf("Responses function_call items require string 'call_id', 'name', and 'arguments'.")
	}
	return map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id":   callID,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": arguments,
			},
		}},
	}, nil
}

func appendFunctionCall(messages []any, message map[string]any) []any {
	if len(messages) == 0 {
		return append(messages, message)
	}
	previous, ok := messages[len(messages)-1].(map[string]any)
	if !ok || previous["role"] != "assistant" || previous["content"] != nil {
		return append(messages, message)
	}
	previousCalls, previousOK := previous["tool_calls"].([]any)
	newCalls, newOK := message["tool_calls"].([]any)
	if !previousOK || !newOK {
		return append(messages, message)
	}
	previous["tool_calls"] = append(previousCalls, newCalls...)
	return messages
}

func responsesFunctionCallOutput(item map[string]any) (map[string]any, error) {
	callID, ok := item["call_id"].(string)
	if !ok || callID == "" {
		return nil, fmt.Errorf("Responses function_call_output items require string 'call_id'.")
	}
	output, ok := item["output"]
	if !ok {
		return nil, fmt.Errorf("Responses function_call_output items require 'output'.")
	}
	return map[string]any{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      output,
	}, nil
}

func responsesTools(tools any) ([]any, error) {
	items, ok := tools.([]any)
	if !ok {
		return nil, fmt.Errorf("Invalid parameter: 'tools' must be an array.")
	}
	converted := make([]any, 0, len(items))
	for _, rawTool := range items {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Invalid tool: expected an object.")
		}
		toolType, _ := tool["type"].(string)
		if toolType != "function" {
			return nil, fmt.Errorf("Unsupported Responses tool type %q.", toolType)
		}
		name, ok := tool["name"].(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("Responses function tools require a non-empty string 'name'.")
		}
		function := map[string]any{"name": name}
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				function[key] = value
			}
		}
		converted = append(converted, map[string]any{
			"type":     "function",
			"function": function,
		})
	}
	return converted, nil
}

func forwardInputTokensError(w http.ResponseWriter, resp *http.Response) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInputTokensErrorBytes+1))
	if err != nil {
		jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return
	}
	if len(body) > maxInputTokensErrorBytes {
		jsonError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}
