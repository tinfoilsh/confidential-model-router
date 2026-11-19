package main

import (
	"net/http"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

const (
	singleCallRequestHeader = "X-Tinfoil-Require-Single-Call"
	maxInputTokensHeader    = "X-Tinfoil-Max-Input-Tokens"
	maxInputTokensField     = "max_input_tokens"
)

// boundedChatBackend is the backend capability boundary. It must only approve
// the selected enclave when the identity used by its verified transport is
// known to enforce the private input cap on the prompt submitted for generation.
// A model name alone is not sufficient. No production backend is wired yet.
type boundedChatBackend func(model string, selected *manager.Enclave) bool

type boundedChatLimits struct {
	input int64
}

func parseBoundedChat(r *http.Request) (*boundedChatLimits, *manager.APIError) {
	invalid := manager.ErrInvalidRequest
	mode := r.Header.Values(singleCallRequestHeader)
	if len(mode) == 0 {
		if len(r.Header.Values(maxInputTokensHeader)) != 0 {
			return nil, invalid.WithMessage("%s requires %s: true.", maxInputTokensHeader, singleCallRequestHeader)
		}
		return nil, nil
	}
	if len(mode) != 1 || strings.TrimSpace(mode[0]) != "true" {
		return nil, invalid.WithMessage("%s must be a single true value.", singleCallRequestHeader)
	}
	const chatPath = "/v1/chat/completions"
	if r.Method != http.MethodPost || r.URL.Path != chatPath || r.URL.EscapedPath() != chatPath || r.Header.Get("Upgrade") != "" {
		return nil, invalid.WithMessage("Single-call mode only supports POST /v1/chat/completions.")
	}
	for _, line := range r.Header.Values("Connection") {
		for _, token := range strings.Split(line, ",") {
			for _, header := range []string{singleCallRequestHeader, maxInputTokensHeader, manager.MaxOutputTokensRequestHeader, manager.CompletionCountRequestHeader} {
				if strings.EqualFold(strings.TrimSpace(token), header) {
					return nil, invalid.WithMessage("Single-call limit headers must not be nominated in Connection.")
				}
			}
		}
	}
	input, ok := positiveIntHeader(r.Header, maxInputTokensHeader)
	if !ok || input == 0 {
		return nil, invalid.WithMessage("%s must be a single positive integer.", maxInputTokensHeader)
	}
	output, ok := positiveIntHeader(r.Header, manager.MaxOutputTokensRequestHeader)
	if !ok || output == 0 {
		return nil, invalid.WithMessage("%s must be a single positive integer.", manager.MaxOutputTokensRequestHeader)
	}
	count, ok := positiveIntHeader(r.Header, manager.CompletionCountRequestHeader)
	if !ok || count != 1 {
		return nil, invalid.WithMessage("%s must be 1 in single-call mode.", manager.CompletionCountRequestHeader)
	}
	return &boundedChatLimits{input: input}, nil
}

func validateBoundedChatBody(body map[string]any, hasRouterTools bool) *manager.APIError {
	invalid := manager.ErrInvalidRequest
	if hasRouterTools {
		return invalid.WithMessage("Single-call mode does not support server-side tools.")
	}
	if _, present := body["best_of"]; present {
		return invalid.WithParam("best_of").WithMessage("Single-call mode does not support best_of.")
	}
	if raw, present := body["modalities"]; present {
		modalities, ok := raw.([]any)
		if !ok || len(modalities) != 1 || modalities[0] != "text" {
			return invalid.WithParam("modalities").WithMessage("Single-call mode only supports text output.")
		}
	}
	if _, present := body["audio"]; present {
		return invalid.WithParam("audio").WithMessage("Single-call mode only supports text output.")
	}
	// Exclude alternate generation, truncation, KV transfer and nontext
	// extensions until their accounting is covered by the backend contract.
	for _, field := range []string{"use_beam_search", "truncate_prompt_tokens", "kv_transfer_params", "prompt_embeds", "multi_modal_data"} {
		if _, present := body[field]; present {
			return invalid.WithParam(field).WithMessage("Single-call mode does not support %s.", field)
		}
	}
	// Reject new backend extensions by default, rather than trying to enumerate
	// every way an engine might do work outside the bounded chat contract.
	for field, value := range body {
		var valid bool
		switch field {
		case "model", "messages", "modalities", "n", "max_completion_tokens", "user_cache_secret",
			"stream", "temperature", "top_p", "stop", "seed", "presence_penalty", "frequency_penalty",
			"logprobs", "top_logprobs", "logit_bias", "reasoning_effort", "parallel_tool_calls", "user":
			valid = true
		case "tools":
			valid = boundedFunctionTools(value)
		case "tool_choice":
			valid = boundedToolChoice(value)
		case "stream_options":
			valid = value == nil || boundedObject(value, "include_usage", "continuous_usage_stats")
		case "response_format":
			valid = boundedResponseFormat(value)
		default:
			return invalid.WithParam(field).WithMessage("Single-call mode does not support %s.", field)
		}
		if !valid {
			return invalid.WithParam(field).WithMessage("Single-call mode does not support this %s shape.", field)
		}
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return invalid.WithParam("messages").WithMessage("Single-call mode requires text messages.")
	}
	for _, raw := range messages {
		if !boundedTextMessage(raw) {
			return invalid.WithParam("messages").WithMessage("Single-call mode only supports text message content and client function tool calls.")
		}
	}
	return nil
}

func boundedObject(value any, fields ...string) bool {
	obj, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key := range obj {
		allowed := false
		for _, field := range fields {
			if key == field {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func boundedNonemptyString(value any) bool {
	text, ok := value.(string)
	return ok && text != ""
}

func boundedFunctionTools(value any) bool {
	tools, ok := value.([]any)
	if !ok {
		return value == nil
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if !boundedObject(raw, "type", "function") || tool["type"] != "function" ||
			!boundedObject(tool["function"], "name", "description", "parameters", "strict") || !boundedNonemptyString(fn["name"]) {
			return false
		}
	}
	return true
}

func boundedToolChoice(value any) bool {
	switch value {
	case nil, "none", "auto", "required":
		return true
	}
	choice, _ := value.(map[string]any)
	fn, _ := choice["function"].(map[string]any)
	return boundedObject(value, "type", "function") && choice["type"] == "function" &&
		boundedObject(choice["function"], "name") && boundedNonemptyString(fn["name"])
}

func boundedResponseFormat(value any) bool {
	if value == nil {
		return true
	}
	format, _ := value.(map[string]any)
	switch format["type"] {
	case "text", "json_object":
		return boundedObject(value, "type")
	case "json_schema":
		return boundedObject(value, "type", "json_schema") &&
			boundedObject(format["json_schema"], "name", "description", "schema", "strict")
	default:
		return false
	}
}

func boundedTextMessage(value any) bool {
	message, _ := value.(map[string]any)
	var hasCalls bool
	switch message["role"] {
	case "system", "developer", "user":
		if !boundedObject(value, "role", "content", "name") {
			return false
		}
	case "assistant":
		if !boundedObject(value, "role", "content", "name", "tool_calls", "refusal", "reasoning_content", "reasoning") {
			return false
		}
		for _, field := range []string{"refusal", "reasoning_content", "reasoning"} {
			if message[field] != nil {
				if _, ok := message[field].(string); !ok {
					return false
				}
			}
		}
		if message["tool_calls"] != nil {
			calls, ok := message["tool_calls"].([]any)
			if !ok {
				return false
			}
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				_, argsOK := fn["arguments"].(string)
				if !boundedObject(raw, "id", "type", "function") || call["type"] != "function" || !boundedNonemptyString(call["id"]) ||
					!boundedObject(call["function"], "name", "arguments") || !boundedNonemptyString(fn["name"]) || !argsOK {
					return false
				}
			}
			hasCalls = len(calls) != 0
		}
	case "tool":
		if !boundedObject(value, "role", "content", "tool_call_id") || !boundedNonemptyString(message["tool_call_id"]) {
			return false
		}
	default:
		return false
	}
	if name, present := message["name"]; present {
		if _, ok := name.(string); !ok {
			return false
		}
	}
	switch content := message["content"].(type) {
	case nil:
		return hasCalls
	case string:
		return true
	case []any:
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			if _, ok := part["text"].(string); !ok || part["type"] != "text" || !boundedObject(raw, "type", "text") {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func prepareBoundedChatDispatch(r *http.Request) {
	r.Header.Del(maxInputTokensHeader)
	r.Header.Del(singleCallRequestHeader)
	// net/http can replay a POST with an idempotency marker and a rewindable
	// body on a stale connection. This path also bypasses replica failover.
	r.GetBody = nil
	r.Header.Del("Idempotency-Key")
	r.Header.Del("X-Idempotency-Key")
}
