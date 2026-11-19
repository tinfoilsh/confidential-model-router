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
	// Exclude alternate generation, truncation, KV transfer and nontext
	// extensions until their accounting is covered by the backend contract.
	for _, field := range []string{"use_beam_search", "truncate_prompt_tokens", "kv_transfer_params", "prompt_embeds", "multi_modal_data"} {
		if _, present := body[field]; present {
			return invalid.WithParam(field).WithMessage("Single-call mode does not support %s.", field)
		}
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return invalid.WithParam("messages").WithMessage("Single-call mode requires text messages.")
	}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		switch content := message["content"].(type) {
		case string:
			continue
		case []any:
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if part["type"] == "text" {
					if _, ok := part["text"].(string); ok {
						continue
					}
				}
				return invalid.WithParam("messages").WithMessage("Single-call mode only supports text message content.")
			}
		default:
			return invalid.WithParam("messages").WithMessage("Single-call mode only supports text message content.")
		}
	}
	return nil
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
