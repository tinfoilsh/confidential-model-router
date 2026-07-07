package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/cachesalt"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

// jwtWithSubject builds an unsigned compact JWT carrying the given subject,
// matching what jwtSubject parses (payload only, signature ignored).
func jwtWithSubject(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	return "hdr." + payload + ".sig"
}

func TestApplyCacheSaltStripsFieldsUnconditionally(t *testing.T) {
	// Stripping must not depend on the flag, the path, or the identity: a
	// client-supplied cache_salt must never pass through, and the
	// user_cache_secret must never reach the engine.
	cases := []struct {
		name    string
		path    string
		apiKey  string
		enabled bool
	}{
		{"disabled", "/v1/chat/completions", "tenant-a", false},
		{"non-allowlisted path", "/v1/embeddings", "tenant-a", true},
		{"empty identity", "/v1/chat/completions", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model":             "m",
				"cache_salt":        "client-chosen",
				"user_cache_secret": "s1",
			}
			mode, changed := applyCacheSalt(body, tc.path, tc.apiKey, tc.enabled)
			if mode != cachesalt.ModeNone {
				t.Errorf("mode = %q, want ModeNone", mode)
			}
			if !changed {
				t.Error("changed = false, want true (fields were present)")
			}
			if _, ok := body["cache_salt"]; ok {
				t.Error("client-supplied cache_salt survived")
			}
			if _, ok := body["user_cache_secret"]; ok {
				t.Error("user_cache_secret survived")
			}
		})
	}
}

func TestApplyCacheSaltChangedFlag(t *testing.T) {
	// A body with neither field, on a non-injecting call, must report no
	// change so verbatim-proxy callers can skip re-marshaling.
	body := map[string]any{"model": "m"}
	if _, changed := applyCacheSalt(body, "/v1/embeddings", "tenant-a", true); changed {
		t.Error("changed = true for a body with no salt fields on a non-injecting path")
	}
	// Injection is itself a change.
	body = map[string]any{"model": "m"}
	if _, changed := applyCacheSalt(body, "/v1/chat/completions", "tenant-a", true); !changed {
		t.Error("changed = false despite injecting a salt")
	}
}

func TestApplyCacheSaltModes(t *testing.T) {
	tenantSalt, _ := cachesalt.Derive("tenant-a", "")
	userSalt, _ := cachesalt.Derive("tenant-a", "s1")

	cases := []struct {
		name     string
		body     map[string]any
		wantSalt string
		wantMode cachesalt.Mode
	}{
		{
			"no secret: tenant namespace",
			map[string]any{"model": "m"},
			tenantSalt, cachesalt.ModeTenant,
		},
		{
			"secret: per-user namespace",
			map[string]any{"model": "m", "user_cache_secret": "s1"},
			userSalt, cachesalt.ModeUser,
		},
		{
			"empty-string secret is absent",
			map[string]any{"model": "m", "user_cache_secret": ""},
			tenantSalt, cachesalt.ModeTenant,
		},
		{
			"null secret is absent",
			map[string]any{"model": "m", "user_cache_secret": nil},
			tenantSalt, cachesalt.ModeTenant,
		},
		{
			"non-string secret is absent",
			map[string]any{"model": "m", "user_cache_secret": float64(42)},
			tenantSalt, cachesalt.ModeTenant,
		},
		{
			"client salt is replaced, not honored",
			map[string]any{"model": "m", "cache_salt": "client-chosen"},
			tenantSalt, cachesalt.ModeTenant,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, _ := applyCacheSalt(tc.body, "/v1/chat/completions", "tenant-a", true)
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if got, _ := tc.body["cache_salt"].(string); got != tc.wantSalt {
				t.Errorf("cache_salt = %q, want %q", got, tc.wantSalt)
			}
			if _, ok := tc.body["user_cache_secret"]; ok {
				t.Error("user_cache_secret survived")
			}
		})
	}
}

func TestApplyCacheSaltEndpointAllowlist(t *testing.T) {
	cases := []struct {
		path   string
		inject bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/completions", true},
		{"/v1/responses", true},
		{"/v1/embeddings", false},
		{"/v1/audio/speech", false},
		{"/v1/convert/file", false},
		{"/v1/chat/completions/", false}, // exact match only
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			body := map[string]any{"model": "m"}
			mode, _ := applyCacheSalt(body, tc.path, "tenant-a", true)
			_, injected := body["cache_salt"]
			if injected != tc.inject {
				t.Errorf("injected = %v, want %v", injected, tc.inject)
			}
			if tc.inject && mode == cachesalt.ModeNone {
				t.Error("injected but mode is ModeNone")
			}
			if !tc.inject && mode != cachesalt.ModeNone {
				t.Errorf("not injected but mode = %q", mode)
			}
		})
	}
}

// TestApplyCacheSaltAnchorsToJWTSubject pins that identity anchoring happens
// inside applyCacheSalt: a JWT bearer must salt on its stable subject, not on
// the raw (rotating) token — otherwise the namespace cold-starts on every
// token refresh. This is the guard against the call site being wired with the
// raw key.
func TestApplyCacheSaltAnchorsToJWTSubject(t *testing.T) {
	token := jwtWithSubject("org_1")
	wantSubjectSalt, _ := cachesalt.Derive("org_1", "")
	wantRawSalt, _ := cachesalt.Derive(token, "")
	if wantSubjectSalt == wantRawSalt {
		t.Fatal("test setup: subject and raw-token salts coincide")
	}

	body := map[string]any{"model": "m"}
	applyCacheSalt(body, "/v1/chat/completions", token, true)
	if got, _ := body["cache_salt"].(string); got != wantSubjectSalt {
		t.Errorf("cache_salt = %q, want the subject-anchored salt %q", got, wantSubjectSalt)
	}
}

// TestApplyCacheSaltOpaqueKeyIdentity covers callers without a JWT: they are
// identified by the opaque API key (rateLimitIdentity's fallback), and the
// salt must anchor to it.
func TestApplyCacheSaltOpaqueKeyIdentity(t *testing.T) {
	key := "tk_live_9f8e7d6c5b4a3210"
	want, _ := cachesalt.Derive(key, "")
	body := map[string]any{"model": "m"}
	applyCacheSalt(body, "/v1/chat/completions", key, true)
	if got, _ := body["cache_salt"].(string); got != want {
		t.Errorf("cache_salt = %q, want %q", got, want)
	}
}

// TestSaltProxiedBody covers the subdomain routing path, where the body is
// otherwise forwarded verbatim.
func TestSaltProxiedBody(t *testing.T) {
	tenantSalt, _ := cachesalt.Derive("tenant-a", "")

	t.Run("injects and strips on the salted body", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m","cache_salt":"client-chosen","user_cache_secret":"s1"}`))
		mode, err := saltProxiedBody(req, "tenant-a", true)
		if err != nil {
			t.Fatalf("saltProxiedBody: %v", err)
		}
		userSalt, _ := cachesalt.Derive("tenant-a", "s1")
		if mode != cachesalt.ModeUser {
			t.Errorf("mode = %q, want ModeUser", mode)
		}
		got := decodeBody(t, req)
		if got["cache_salt"] != userSalt {
			t.Errorf("cache_salt = %v, want %q", got["cache_salt"], userSalt)
		}
		if _, ok := got["user_cache_secret"]; ok {
			t.Error("user_cache_secret survived to the engine")
		}
	})

	t.Run("strips client salt even when disabled", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m","cache_salt":"client-chosen"}`))
		mode, err := saltProxiedBody(req, "tenant-a", false)
		if err != nil {
			t.Fatalf("saltProxiedBody: %v", err)
		}
		if mode != cachesalt.ModeNone {
			t.Errorf("mode = %q, want ModeNone", mode)
		}
		if got := decodeBody(t, req); got["cache_salt"] != nil {
			t.Errorf("client cache_salt survived while disabled: %v", got["cache_salt"])
		}
	})

	t.Run("injects tenant salt with no secret", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m"}`))
		mode, err := saltProxiedBody(req, "tenant-a", true)
		if err != nil {
			t.Fatalf("saltProxiedBody: %v", err)
		}
		if mode != cachesalt.ModeTenant {
			t.Errorf("mode = %q, want ModeTenant", mode)
		}
		if got := decodeBody(t, req); got["cache_salt"] != tenantSalt {
			t.Errorf("cache_salt = %v, want %q", got["cache_salt"], tenantSalt)
		}
	})

	t.Run("leaves an unchanged body byte-identical", func(t *testing.T) {
		const raw = `{"model":"m","messages":[]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw))
		// Disabled + no salt fields: nothing to do, must not re-marshal.
		if _, err := saltProxiedBody(req, "tenant-a", false); err != nil {
			t.Fatalf("saltProxiedBody: %v", err)
		}
		out, _ := io.ReadAll(req.Body)
		if string(out) != raw {
			t.Errorf("body was re-marshaled: got %q, want %q", out, raw)
		}
	})

	t.Run("preserves int64 precision through the re-marshal", func(t *testing.T) {
		// 2^53+1 is not representable as float64; default JSON decoding
		// would silently turn it into ...992.
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m","seed":9007199254740993}`))
		if _, err := saltProxiedBody(req, "tenant-a", true); err != nil {
			t.Fatalf("saltProxiedBody: %v", err)
		}
		raw, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(raw), `"seed":9007199254740993`) {
			t.Errorf("seed lost precision: %s", raw)
		}
	})

	t.Run("forwards a non-JSON body untouched", func(t *testing.T) {
		const raw = `not json`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw))
		mode, err := saltProxiedBody(req, "tenant-a", true)
		if err != nil {
			t.Fatalf("saltProxiedBody: %v", err)
		}
		if mode != cachesalt.ModeNone {
			t.Errorf("mode = %q, want ModeNone", mode)
		}
		out, _ := io.ReadAll(req.Body)
		if string(out) != raw {
			t.Errorf("non-JSON body altered: got %q", out)
		}
	})

	t.Run("forwards a body with trailing data untouched", func(t *testing.T) {
		// json.Unmarshal rejects all of these, and so must the engine-bound
		// rewrite: re-marshaling only the first value would silently convert
		// a request the engine rejects into one it accepts. The '}'/' ]'
		// cases are the regression: dec.More() reports "no more elements" at
		// either byte, so they used to slip past the trailing-data guard.
		for _, raw := range []string{
			`{"model":"m","cache_salt":"evil"}}`,
			`{"model":"m"}]`,
			`{"model":"m"}} garbage`,
			`{"model":"m"}{"model":"n"}`,
			`{"model":"m"} x`,
		} {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw))
			mode, err := saltProxiedBody(req, "tenant-a", true)
			if err != nil {
				t.Fatalf("saltProxiedBody(%q): %v", raw, err)
			}
			if mode != cachesalt.ModeNone {
				t.Errorf("mode = %q for %q, want ModeNone", mode, raw)
			}
			out, _ := io.ReadAll(req.Body)
			if string(out) != raw {
				t.Errorf("trailing-data body altered: got %q, want %q", out, raw)
			}
		}
	})

	t.Run("trailing whitespace is not trailing data", func(t *testing.T) {
		// json.Unmarshal accepts trailing whitespace, so the strictness fix
		// must not regress ordinary clients that end the body with a newline.
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader("{\"model\":\"m\"}\n\t "))
		mode, err := saltProxiedBody(req, "tenant-a", true)
		if err != nil {
			t.Fatalf("saltProxiedBody: %v", err)
		}
		if mode != cachesalt.ModeTenant {
			t.Errorf("mode = %q, want ModeTenant", mode)
		}
		if got := decodeBody(t, req); got["cache_salt"] != tenantSalt {
			t.Errorf("cache_salt = %v, want %q", got["cache_salt"], tenantSalt)
		}
	})
}

// TestCacheSaltMetricSkippedInjection pins that a skipped injection emits no
// metric sample — dashboards must never see an empty mode label.
func TestCacheSaltMetricSkippedInjection(t *testing.T) {
	before := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal)

	// Skipped: empty identity on an allowlisted, enabled path.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	mode, err := saltProxiedBody(req, "", true)
	if err != nil {
		t.Fatalf("saltProxiedBody: %v", err)
	}
	if mode != cachesalt.ModeNone {
		t.Fatalf("mode = %q, want ModeNone", mode)
	}
	// The handler only increments when mode != ModeNone; assert the guard by
	// confirming no new series would be minted for an empty label here.
	if after := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal); after != before {
		t.Errorf("metric series changed on a skipped injection: %d -> %d", before, after)
	}
}

// TestRecordCacheSaltInjection pins the guard that both routing branches rely
// on: ModeNone emits no sample (an empty mode label must never exist), any
// real mode counts one under exactly {model, mode}.
func TestRecordCacheSaltInjection(t *testing.T) {
	before := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal)

	recordCacheSaltInjection("record-metric-skip", cachesalt.ModeNone)
	if after := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal); after != before {
		t.Errorf("ModeNone minted a series: %d -> %d", before, after)
	}

	recordCacheSaltInjection("record-metric-count", cachesalt.ModeTenant)
	recordCacheSaltInjection("record-metric-count", cachesalt.ModeUser)
	recordCacheSaltInjection("record-metric-count", cachesalt.ModeUser)
	if got := testutil.ToFloat64(manager.CacheSaltInjectionsTotal.WithLabelValues("record-metric-count", "tenant")); got != 1 {
		t.Errorf(`series {record-metric-count,tenant} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(manager.CacheSaltInjectionsTotal.WithLabelValues("record-metric-count", "user")); got != 2 {
		t.Errorf(`series {record-metric-count,user} = %v, want 2`, got)
	}
}

// decodeBody reads the request's current Body (the value saltProxiedBody
// rewrote it to) and parses it as JSON.
func decodeBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal body %q: %v", raw, err)
	}
	return m
}
