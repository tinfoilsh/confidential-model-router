package toolruntime

import (
	"net/url"
	"strings"
	"unicode"
)

// citationTrackingParams lists the query-string keys whose presence or
// absence does not change which resource a URL identifies. They are
// dropped from the URL before comparison so a citation that carries
// `?utm_source=exa` still matches the canonical copy returned by the
// search tool.
var citationTrackingParams = map[string]struct{}{
	"utm_source":   {},
	"utm_medium":   {},
	"utm_campaign": {},
	"utm_term":     {},
	"utm_content":  {},
	"utm_id":       {},
	"fbclid":       {},
	"gclid":        {},
	"mc_cid":       {},
	"mc_eid":       {},
	"ref":          {},
	"ref_src":      {},
}

// normalizeCitationURL reduces a URL to a canonical form used as the
// citation-match key. The returned string identifies the same resource
// across the cosmetic differences that real-world search output and
// model-emitted markdown disagree on: trailing slashes, `www.` host
// prefixes, scheme/host casing, tracking query params, fragments, and
// invisible Unicode characters that tokenizers sometimes smuggle onto
// URL tails.
//
// The original URL string is never mutated; only the match key is
// normalized. Annotations still ship the URL the search tool returned
// so downstream renderers continue to receive the canonical form the
// source advertised.
//
// Inputs that do not parse as URLs are returned unchanged so the lookup
// falls back to exact-string equality for those pathological cases
// instead of silently collapsing distinct strings.
func normalizeCitationURL(raw string) string {
	raw = stripInvisibleRunes(raw)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Host = strings.TrimPrefix(parsed.Host, "www.")

	if len(parsed.Path) > 1 {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}

	if query := parsed.Query(); len(query) > 0 {
		for key := range query {
			if _, drop := citationTrackingParams[strings.ToLower(key)]; drop {
				query.Del(key)
			}
		}
		parsed.RawQuery = query.Encode()
	}

	parsed.Fragment = ""
	parsed.RawFragment = ""

	return parsed.String()
}

// stripInvisibleRunes removes Unicode format and zero-width characters
// that models occasionally emit at URL boundaries. These are visually
// absent but make two otherwise identical strings compare unequal under
// byte or rune equality.
func stripInvisibleRunes(raw string) string {
	if raw == "" {
		return raw
	}
	for _, r := range raw {
		if shouldStripRune(r) {
			goto rebuild
		}
	}
	return raw

rebuild:
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if shouldStripRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func shouldStripRune(r rune) bool {
	switch r {
	case '\u200B', '\u200C', '\u200D', '\u200E', '\u200F',
		'\u2028', '\u2029', '\u202A', '\u202B', '\u202C', '\u202D', '\u202E',
		'\u2060', '\u2061', '\u2062', '\u2063', '\u2064',
		'\uFEFF':
		return true
	}
	return unicode.Is(unicode.Cf, r)
}
