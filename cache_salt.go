package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/tinfoilsh/confidential-model-router/cachesalt"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

// cacheSaltPaths are the endpoints whose engine request schema accepts a
// cache_salt field. Pooling endpoints (embeddings) do not and would reject
// the unknown field.
var cacheSaltPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/responses":        true,
}

// errBodyNotObject is returned when a proxied body decodes but is not a
// single JSON object. Its text is shown to the client.
var errBodyNotObject = errors.New("request body must be one JSON object")

// applyCacheSalt owns the two cache-salt request fields on a parsed body. It
// always pops user_cache_secret (router-only input to salt derivation; never
// sent to the engine) and strips any client-supplied cache_salt — the salt
// decides who shares the engine's prefix cache, so a client must never
// choose it. A non-string user_cache_secret (null, number, object) is
// treated as absent.
//
// When enabled, the endpoint supports the field, and the caller resolves to
// a non-empty identity, it injects the derived salt into body. It returns
// the derivation mode (ModeNone if no salt was injected).
//
// apiKey is the raw bearer token; identity anchoring (JWT subject, else the
// opaque key) happens here via cacheSaltIdentity so the call site cannot
// wire the wrong value.
func applyCacheSalt(body map[string]any, path, apiKey string, enabled bool) cachesalt.Mode {
	// An absent body has no fields to strip or salt.
	if body == nil {
		return cachesalt.ModeNone
	}
	secret, _ := body["user_cache_secret"].(string)
	delete(body, "user_cache_secret")
	delete(body, "cache_salt")

	if !enabled || !cacheSaltPaths[path] {
		return cachesalt.ModeNone
	}
	salt, mode := cachesalt.Derive(cacheSaltIdentity(apiKey), secret)
	if salt == "" {
		return cachesalt.ModeNone
	}
	body["cache_salt"] = salt
	return mode
}

// decodeJSONBody preserves exact numbers (such as int64 seeds) when the
// router rewrites a request. Require one object, with no trailing data.
func decodeJSONBody(bodyBytes []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	if !decodeConsumedAll(dec) || body == nil {
		return nil, errBodyNotObject
	}
	return body, nil
}

// recordCacheSaltInjection counts a performed injection. A skipped one
// (ModeNone) emits no sample: dashboards distinguish "salting off" from
// "salting on, mode X" by series existence, so an empty mode label must
// never appear. Keeping the guard here, next to the code that produces
// Mode, means no call site can mint one by forgetting it.
func recordCacheSaltInjection(modelName string, mode cachesalt.Mode) {
	if mode == cachesalt.ModeNone {
		return
	}
	manager.CacheSaltInjectionsTotal.WithLabelValues(modelName, string(mode)).Inc()
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

func replaceJSONBody(r *http.Request, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	return nil
}
