package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// maxOutputTokensFields lists, per generation endpoint, the body fields the
// engine reads as the output-token limit. The first field is the one written
// when the body sets none. Chat completions accepts both fields and the
// engine prefers max_completion_tokens, so every field that is set must agree
// with the header or a client could bypass it through the other one.
var maxOutputTokensFields = map[string][]string{
	"/v1/chat/completions": {"max_completion_tokens", "max_tokens"},
	"/v1/completions":      {"max_tokens"},
	"/v1/responses":        {"max_output_tokens"},
}

// applyMaxOutputTokensHeader enforces the optional X-Tinfoil-Max-Output-Tokens
// header on generation endpoints. EHBP leaves headers readable to proxies in
// front of the enclave while the body stays sealed, so the header lets such a
// proxy know the output limit, and this check guarantees the body honors it:
// when the body sets no limit the header value is written into it, and when
// it sets one that differs the request is rejected. Other endpoints ignore
// the header, and an empty header counts as absent. body must come from
// decodeJSONBody so numbers are json.Number.
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

	limited := false
	for _, field := range fields {
		value, present := body[field]
		if !present || value == nil {
			continue
		}
		number, _ := value.(json.Number)
		if n, err := number.Int64(); err != nil || n != limit {
			return manager.ErrMaxTokensMismatch.WithParam(field).WithMessage(manager.ErrMsgMaxTokensMismatch, field)
		}
		limited = true
	}
	if !limited {
		body[fields[0]] = limit
	}
	return nil
}
