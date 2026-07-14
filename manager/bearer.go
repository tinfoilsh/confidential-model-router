package manager

import "strings"

// BearerToken extracts the token from an Authorization header value,
// accepting the scheme case-insensitively (RFC 7235) and trimming
// surrounding whitespace, matching how the outer shim parses the header.
// Every Authorization-header consumer (identity, cache salting, billing)
// must share this parse: a stricter one would treat shim-authenticated
// traffic as anonymous. Returns "" when the header carries no bearer token.
func BearerToken(header string) string {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(header[len(scheme):])
}
