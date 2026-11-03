package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// applyCompletionCountHeader enforces the optional X-Tinfoil-Completion-Count
// header on chat completions. Like the output-token header, it lets an EHBP
// proxy see a limit the enclave enforces over the sealed body: the header
// replaces any n the body sets. Other endpoints ignore the header.
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
	body["n"] = count
	return nil
}
