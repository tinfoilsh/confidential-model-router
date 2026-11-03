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
// body cannot exceed it. Other endpoints ignore the header.
func applyMaxOutputTokensHeader(header http.Header, path string, body map[string]any) *manager.APIError {
	fields, ok := maxOutputTokensFields[path]
	if !ok {
		return nil
	}
	limit, ok := positiveIntHeader(header, manager.MaxOutputTokensRequestHeader)
	if !ok {
		return manager.ErrInvalidRequest.WithMessage(manager.ErrMsgInvalidMaxTokensHeader)
	}
	if limit == 0 {
		return nil
	}

	for _, field := range fields {
		delete(body, field)
	}
	body[fields[0]] = limit
	return nil
}

// positiveIntHeader reads an optional header that must hold exactly one
// positive base-10 integer. An absent header returns 0 and ok; ok is false
// only when the header is present with any other value.
func positiveIntHeader(header http.Header, name string) (value int64, ok bool) {
	values := header.Values(name)
	if len(values) == 0 {
		return 0, true
	}
	if len(values) > 1 {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if err != nil || value < 1 {
		return 0, false
	}
	return value, true
}
