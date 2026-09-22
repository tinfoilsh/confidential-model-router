package main

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/cachesalt"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

// jwtWithSubject builds an unsigned at+jwt-shaped token carrying the given
// subject, matching the token type the downstream shims verify.
func jwtWithSubject(sub string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"at+jwt"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	return header + "." + payload + ".sig"
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
			mode := applyCacheSalt(body, tc.path, tc.apiKey, tc.enabled)
			if mode != cachesalt.ModeNone {
				t.Errorf("mode = %q, want ModeNone", mode)
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
			mode := applyCacheSalt(tc.body, "/v1/chat/completions", "tenant-a", true)
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
			mode := applyCacheSalt(body, tc.path, "tenant-a", true)
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
// identified by the opaque API key (cacheSaltIdentity's fallback), and the
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

func TestApplyCacheSaltJWTShapedOpaqueKeyUsesRawIdentity(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"api-key"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"attacker-chosen"}`))
	key := header + "." + payload + ".opaque"
	want, _ := cachesalt.Derive(key, "")
	body := map[string]any{"model": "m"}
	applyCacheSalt(body, "/v1/chat/completions", key, true)
	if got, _ := body["cache_salt"].(string); got != want {
		t.Errorf("cache_salt = %q, want raw opaque-key identity %q", got, want)
	}
}

type countedCloseBody struct {
	io.Reader
	closes int
}

func (b *countedCloseBody) Close() error {
	b.closes++
	return nil
}

func TestReplaceJSONBodyMarshalFailurePreservesRequest(t *testing.T) {
	const raw = `{"model":"gpt-oss-120b"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	req.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	original := &countedCloseBody{Reader: req.Body}
	req.Body = original
	if err := replaceJSONBody(req, map[string]any{"invalid": make(chan struct{})}); err == nil {
		t.Fatal("unsupported JSON value accepted")
	}
	if req.Body != original || original.closes != 0 || req.ContentLength != int64(len(raw)) || req.Header.Get("Content-Length") != strconv.Itoa(len(raw)) {
		t.Fatal("marshal failure mutated request")
	}
	got, err := io.ReadAll(req.Body)
	if err != nil || string(got) != raw {
		t.Fatalf("original body=%q error=%v", got, err)
	}
}

// TestCacheSaltMetricSkippedInjection pins that a skipped injection emits no
// metric sample — dashboards must never see an empty mode label.
func TestCacheSaltMetricSkippedInjection(t *testing.T) {
	before := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal)

	// Skipped: empty identity on an allowlisted, enabled path.
	mode := applyCacheSalt(map[string]any{"model": "m"}, "/v1/chat/completions", "", true)
	recordCacheSaltInjection("skipped-injection", mode)
	if mode != cachesalt.ModeNone {
		t.Fatalf("mode = %q, want ModeNone", mode)
	}
	// The handler only increments when mode != ModeNone; assert the guard by
	// confirming no new series would be minted for an empty label here.
	if after := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal); after != before {
		t.Errorf("metric series changed on a skipped injection: %d -> %d", before, after)
	}
}

// TestRecordCacheSaltInjection pins the metric guard: ModeNone emits no
// sample; any real mode counts one under exactly {model, mode}.
func TestRecordCacheSaltInjection(t *testing.T) {
	before := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal)

	recordCacheSaltInjection("record-metric-skip", cachesalt.ModeNone)
	if after := testutil.CollectAndCount(manager.CacheSaltInjectionsTotal); after != before {
		t.Errorf("ModeNone minted a series: %d -> %d", before, after)
	}

	// The shared counters carry values across test reruns in one process,
	// so assert growth, not absolutes.
	tenantBase := testutil.ToFloat64(manager.CacheSaltInjectionsTotal.WithLabelValues("record-metric-count", "tenant"))
	userBase := testutil.ToFloat64(manager.CacheSaltInjectionsTotal.WithLabelValues("record-metric-count", "user"))
	recordCacheSaltInjection("record-metric-count", cachesalt.ModeTenant)
	recordCacheSaltInjection("record-metric-count", cachesalt.ModeUser)
	recordCacheSaltInjection("record-metric-count", cachesalt.ModeUser)
	if got := testutil.ToFloat64(manager.CacheSaltInjectionsTotal.WithLabelValues("record-metric-count", "tenant")) - tenantBase; got != 1 {
		t.Errorf(`series {record-metric-count,tenant} delta = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(manager.CacheSaltInjectionsTotal.WithLabelValues("record-metric-count", "user")) - userBase; got != 2 {
		t.Errorf(`series {record-metric-count,user} delta = %v, want 2`, got)
	}
}
