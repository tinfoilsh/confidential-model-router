package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// maxOutputTokensFields lists, per generation endpoint, the body fields the
// engine reads as the output-token limit. The first field is the one the
// header value is written to. Chat completions accepts both fields and the
// engine prefers max_completion_tokens, so the others are removed rather than
// left to disagree with it.
var maxOutputTokensFields = map[string][]string{
	"/v1/chat/completions": {"max_completion_tokens", "max_tokens"},
	"/v1/completions":      {"max_tokens"},
	"/v1/responses":        {"max_output_tokens"},
}

// applyMaxOutputTokensHeader enforces the optional X-Tinfoil-Max-Output-Tokens
// header on generation endpoints. EHBP leaves headers readable to proxies in
// front of the enclave while the body stays sealed, so the header lets such a
// proxy know the output limit; it replaces any limit the body sets so the
// body cannot exceed it. Other endpoints ignore the header, and an empty
// header counts as absent.
func applyMaxOutputTokensHeader(header http.Header, path string, body map[string]any) *manager.APIError {
	fields, ok := maxOutputTokensFields[path]
	if !ok {
		return nil
	}
	values := header.Values(manager.MaxOutputTokensRequestHeader)
	if len(values) == 0 {
		return nil
	}
	if len(values) > 1 {
		return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgInvalidMaxTokensHeader)
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" {
		return nil
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit < 1 {
		return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgInvalidMaxTokensHeader)
	}

	for _, field := range fields {
		delete(body, field)
	}
	body[fields[0]] = limit
	return nil
}
