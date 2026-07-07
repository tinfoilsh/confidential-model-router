package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tinfoilsh/confidential-model-router/cachesalt"
)

// cacheSaltPaths are the endpoints whose engine request schema accepts a
// cache_salt field. Pooling endpoints (embeddings) do not and would reject
// the unknown field.
var cacheSaltPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/responses":        true,
}

// applyCacheSalt owns the two cache-salt request fields on a parsed body. It
// always pops user_cache_secret (router-only input to salt derivation; never
// sent to the engine) and strips any client-supplied cache_salt — the salt
// decides who shares the engine's prefix cache, so a client must never
// choose it. A non-string user_cache_secret (null, number, object) is
// treated as absent.
//
// When enabled, the endpoint supports the field, and the caller resolves to
// a non-empty identity, it injects the derived salt into body. It returns
// the derivation mode (ModeNone if no salt was injected) and whether it
// modified body at all, so callers that proxy verbatim can skip a needless
// re-marshal.
//
// apiKey is the raw bearer token; identity anchoring (JWT subject, else the
// opaque key) happens here via rateLimitIdentity so the call site cannot
// wire the wrong value.
func applyCacheSalt(body map[string]any, path, apiKey string, enabled bool) (cachesalt.Mode, bool) {
	// A JSON `null` body unmarshals to a nil map (with no error). It carries
	// no fields to strip and no prompt to cache, and injecting would panic on
	// the nil map — treat it as passthrough. The engine rejects the null body.
	if body == nil {
		return cachesalt.ModeNone, false
	}
	_, hadSecret := body["user_cache_secret"]
	_, hadSalt := body["cache_salt"]
	secret, _ := body["user_cache_secret"].(string)
	delete(body, "user_cache_secret")
	delete(body, "cache_salt")
	changed := hadSecret || hadSalt

	if !enabled || !cacheSaltPaths[path] {
		return cachesalt.ModeNone, changed
	}
	salt, mode := cachesalt.Derive(rateLimitIdentity(apiKey), secret)
	if salt == "" {
		return cachesalt.ModeNone, changed
	}
	body["cache_salt"] = salt
	return mode, true
}

// saltProxiedBody applies cache-salt handling to a request that the router
// otherwise forwards verbatim (the subdomain routing path, which never
// parses the body). It rewrites r.Body in place and returns the derivation
// mode. Only JSON-object bodies are touched; a non-JSON body is forwarded
// byte-for-byte, and a body that needs no change is not re-marshaled.
func saltProxiedBody(r *http.Request, apiKey string, enabled bool) (cachesalt.Mode, error) {
	bodyBytes, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return cachesalt.ModeNone, err
	}

	// UseNumber keeps numbers as their exact text across the re-marshal;
	// the default float64 decoding silently corrupts int64-range values
	// (e.g. seed). decodeConsumedAll rejects trailing data, matching
	// json.Unmarshal's single-document strictness, so a body the engine
	// would reject is never re-marshaled into one it accepts.
	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil || !decodeConsumedAll(dec) {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return cachesalt.ModeNone, nil
	}

	mode, changed := applyCacheSalt(body, r.URL.Path, apiKey, enabled)
	if !changed {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return mode, nil
	}

	newBytes, err := json.Marshal(body)
	if err != nil {
		return cachesalt.ModeNone, err
	}
	r.Body = io.NopCloser(bytes.NewReader(newBytes))
	r.ContentLength = int64(len(newBytes))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(newBytes)))
	return mode, nil
}

// decodeConsumedAll reports whether dec has nothing left but trailing
// whitespace: a follow-up Token read returns io.EOF only at true end of
// input. dec.More() is not enough here — it exists to iterate elements
// inside a container and reports "no more elements" at a '}' or ']', so a
// body like `{...}}` would slip past it and be re-marshaled without its
// trailing bytes, quietly converting a request the engine rejects into one
// it accepts.
func decodeConsumedAll(dec *json.Decoder) bool {
	_, err := dec.Token()
	return err == io.EOF
}
