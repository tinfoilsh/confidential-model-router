package main

import (
	"net/http"

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
	count, ok := positiveIntHeader(header, manager.CompletionCountRequestHeader)
	if !ok {
		return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgInvalidCompletionCountHeader)
	}
	if count == 0 {
		return nil
	}
	body["n"] = count
	return nil
}
