package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// applyCompletionCountHeader overrides n on chat completions when the optional
// visible header is supplied. Like the output-token header, this lets an EHBP
// proxy see a limit the enclave enforces over the sealed body. Neither header
// is a request-total budget across server-side tool loops or repeated calls.
// Without this header, existing body semantics are unchanged.
func applyCompletionCountHeader(header http.Header, path string, body map[string]any) *manager.APIError {
	if path != "/v1/chat/completions" {
		return nil
	}
	values := header.Values(manager.CompletionCountRequestHeader)
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgInvalidCompletionCountHeader)
	}
	count, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if err != nil || count < 1 {
		return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgInvalidCompletionCountHeader)
	}
	// best_of may generate candidates beyond those returned in n. Do not let
	// a sealed body hide that additional work behind the visible count.
	// decodeJSONBody preserves numbers as json.Number.
	if raw, present := body["best_of"]; present {
		number, ok := raw.(json.Number)
		if !ok {
			return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgCompletionCountBestOf)
		}
		bestOf, err := number.Int64()
		if err != nil || bestOf != 1 {
			return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgCompletionCountBestOf)
		}
		// Redundant once validated; remove it so a backend cannot interpret
		// best_of=1 as conflicting with an enforced n greater than one.
		delete(body, "best_of")
	}
	body["n"] = count
	return nil
}
