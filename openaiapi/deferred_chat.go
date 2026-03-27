package openaiapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const deferredChatMarkerPrefix = "<|tinfoil_router_deferred_chat_fields_v1|>"

type deferredChatPayload struct {
	CodeInterpreterOptions json.RawMessage `json:"code_interpreter_options,omitempty"`
	Tools                  json.RawMessage `json:"tools,omitempty"`
	ToolChoice             json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls      json.RawMessage `json:"parallel_tool_calls,omitempty"`
	Functions              json.RawMessage `json:"functions,omitempty"`
	FunctionCall           json.RawMessage `json:"function_call,omitempty"`
}

func (r *Request) PrepareDeferredChatFieldsForWebSearch() error {
	if r == nil || r.Kind != EndpointChatCompletions || r.Chat == nil {
		return nil
	}
	if !hasNonNullField(r.RawFields, "web_search_options") || !hasNonNullField(r.RawFields, "code_interpreter_options") {
		return nil
	}

	fields := cloneRawFields(r.RawFields)
	changed, err := embedDeferredChatPayload(fields)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	r.RawFields = fields
	r.modified = true
	r.Chat.CodeInterpreterOptions = nil
	return nil
}

func restoreDeferredChatFields(fields map[string]json.RawMessage) (bool, error) {
	if fields == nil {
		return false, nil
	}

	messagesRaw, ok := fields["messages"]
	if !ok || len(messagesRaw) == 0 {
		return false, nil
	}

	restoredMessages, payload, found, err := stripDeferredChatPayload(messagesRaw)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	fields["messages"] = restoredMessages
	if len(payload.CodeInterpreterOptions) > 0 && !hasNonNullField(fields, "code_interpreter_options") {
		fields["code_interpreter_options"] = cloneRawMessage(payload.CodeInterpreterOptions)
	}
	if len(payload.Tools) > 0 && !hasNonNullField(fields, "tools") {
		fields["tools"] = cloneRawMessage(payload.Tools)
	}
	if len(payload.ToolChoice) > 0 && !hasNonNullField(fields, "tool_choice") {
		fields["tool_choice"] = cloneRawMessage(payload.ToolChoice)
	}
	if len(payload.ParallelToolCalls) > 0 && !hasNonNullField(fields, "parallel_tool_calls") {
		fields["parallel_tool_calls"] = cloneRawMessage(payload.ParallelToolCalls)
	}
	if len(payload.Functions) > 0 && !hasNonNullField(fields, "functions") {
		fields["functions"] = cloneRawMessage(payload.Functions)
	}
	if len(payload.FunctionCall) > 0 && !hasNonNullField(fields, "function_call") {
		fields["function_call"] = cloneRawMessage(payload.FunctionCall)
	}

	return true, nil
}

func embedDeferredChatPayload(fields map[string]json.RawMessage) (bool, error) {
	messagesRaw, ok := fields["messages"]
	if !ok || len(messagesRaw) == 0 {
		return false, fmt.Errorf("missing required parameter: 'messages'")
	}

	messages, err := rawArray(messagesRaw)
	if err != nil {
		return false, fmt.Errorf("invalid parameter: 'messages' must be an array")
	}
	if hasDeferredChatMarker(messages) {
		return false, nil
	}

	payloadBytes, err := json.Marshal(deferredChatPayload{
		CodeInterpreterOptions: cloneRawMessage(fields["code_interpreter_options"]),
		Tools:                  cloneRawMessage(fields["tools"]),
		ToolChoice:             cloneRawMessage(fields["tool_choice"]),
		ParallelToolCalls:      cloneRawMessage(fields["parallel_tool_calls"]),
		Functions:              cloneRawMessage(fields["functions"]),
		FunctionCall:           cloneRawMessage(fields["function_call"]),
	})
	if err != nil {
		return false, err
	}

	markerContent := deferredChatMarkerPrefix + base64.RawURLEncoding.EncodeToString(payloadBytes)
	markerMessage, err := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": markerContent,
	})
	if err != nil {
		return false, err
	}

	updated := make([]json.RawMessage, 0, len(messages)+1)
	updated = append(updated, markerMessage)
	for _, message := range messages {
		updated = append(updated, cloneRawMessage(message))
	}

	encoded, err := json.Marshal(updated)
	if err != nil {
		return false, err
	}

	fields["messages"] = encoded
	delete(fields, "code_interpreter_options")
	return true, nil
}

func stripDeferredChatPayload(messagesRaw json.RawMessage) (json.RawMessage, deferredChatPayload, bool, error) {
	messages, err := rawArray(messagesRaw)
	if err != nil {
		return nil, deferredChatPayload{}, false, fmt.Errorf("invalid parameter: 'messages' must be an array")
	}

	var (
		payload deferredChatPayload
		found   bool
	)
	remaining := make([]json.RawMessage, 0, len(messages))
	for _, rawMessage := range messages {
		isMarker, decoded, err := parseDeferredChatMarker(rawMessage)
		if err != nil {
			return nil, deferredChatPayload{}, false, err
		}
		if !isMarker {
			remaining = append(remaining, cloneRawMessage(rawMessage))
			continue
		}
		if found {
			continue
		}
		payload = decoded
		found = true
	}

	if !found {
		return messagesRaw, deferredChatPayload{}, false, nil
	}

	encoded, err := json.Marshal(remaining)
	if err != nil {
		return nil, deferredChatPayload{}, false, err
	}
	return encoded, payload, true, nil
}

func hasDeferredChatMarker(messages []json.RawMessage) bool {
	for _, rawMessage := range messages {
		isMarker, _, err := parseDeferredChatMarker(rawMessage)
		if err == nil && isMarker {
			return true
		}
	}
	return false
}

func parseDeferredChatMarker(rawMessage json.RawMessage) (bool, deferredChatPayload, error) {
	fields, err := rawObjectFields(rawMessage)
	if err != nil {
		return false, deferredChatPayload{}, nil
	}
	if fields == nil {
		return false, deferredChatPayload{}, nil
	}

	var role string
	if err := json.Unmarshal(fields["role"], &role); err != nil {
		return false, deferredChatPayload{}, nil
	}
	if strings.TrimSpace(role) != "assistant" {
		return false, deferredChatPayload{}, nil
	}

	var content string
	if err := json.Unmarshal(fields["content"], &content); err != nil {
		return false, deferredChatPayload{}, nil
	}
	if !strings.HasPrefix(content, deferredChatMarkerPrefix) {
		return false, deferredChatPayload{}, nil
	}

	decodedBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(content, deferredChatMarkerPrefix))
	if err != nil {
		return false, deferredChatPayload{}, fmt.Errorf("invalid deferred chat payload")
	}

	var payload deferredChatPayload
	if err := json.Unmarshal(decodedBytes, &payload); err != nil {
		return false, deferredChatPayload{}, fmt.Errorf("invalid deferred chat payload")
	}
	return true, payload, nil
}
