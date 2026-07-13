package cacheroute

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/cachesalt"
	"github.com/tinfoilsh/confidential-model-router/config"
)

const testSalt = "0000000000000000000000000000000000000000000" // 43 chars, like a real salt

// chatBody builds a chat-completions body whose prompt clears the default
// eligibility floor.
func chatBody(system, user string) map[string]any {
	return map[string]any{
		"model": "test",
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": user},
		},
	}
}

func bigChatBody() map[string]any {
	return chatBody(strings.Repeat("s", 3000), strings.Repeat("u", 3000))
}

func defaultSettings() Settings {
	return SettingsFrom(nil)
}

func extract(t *testing.T, body map[string]any, path, salt string) *Request {
	t.Helper()
	req := ExtractRequest(body, path, salt, defaultSettings())
	if req.Outcome != OutcomeKeyed {
		t.Fatalf("fixture not keyed: %s (prompt too small for the floor?)", req.Outcome)
	}
	return req
}

func TestExtractRequestFunnel(t *testing.T) {
	small := chatBody("sys", "hi")

	tests := []struct {
		name    string
		body    map[string]any
		salt    string
		outcome Outcome
	}{
		{"no salt", bigChatBody(), "", OutcomeNoSalt},
		{"no prompt fields", map[string]any{"model": "test"}, testSalt, OutcomeNoPrefix},
		{"below floor", small, testSalt, OutcomeBelowFloor},
		{"prompt_cache_key overrides floor", withCacheKey(small, "grp"), testSalt, OutcomeKeyed},
		{"eligible", bigChatBody(), testSalt, OutcomeKeyed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ExtractRequest(tt.body, "/v1/chat/completions", tt.salt, defaultSettings())
			if req.Outcome != tt.outcome {
				t.Fatalf("outcome = %s, want %s", req.Outcome, tt.outcome)
			}
		})
	}
}

func withCacheKey(body map[string]any, key string) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	out["prompt_cache_key"] = key
	return out
}

// TestExtractRequestKeyComponents verifies that each key input moves the key
// and that non-prompt fields do not.
func TestExtractRequestKeyComponents(t *testing.T) {
	base := extract(t, bigChatBody(), "/v1/chat/completions", testSalt).Key

	// Each input changes the key.
	otherSalt := extract(t, bigChatBody(), "/v1/chat/completions", strings.Repeat("1", 43)).Key
	if otherSalt == base {
		t.Error("different salt must change the key")
	}
	withKey := extract(t, withCacheKey(bigChatBody(), "grp"), "/v1/chat/completions", testSalt).Key
	if withKey == base {
		t.Error("prompt_cache_key must change the key")
	}
	otherSystem := bigChatBody()
	otherSystem["messages"].([]any)[0].(map[string]any)["content"] = strings.Repeat("S", 3000)
	if extract(t, otherSystem, "/v1/chat/completions", testSalt).Key == base {
		t.Error("different system prompt must change the key")
	}
	withTools := bigChatBody()
	withTools["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}}
	if extract(t, withTools, "/v1/chat/completions", testSalt).Key == base {
		t.Error("tools must change the key")
	}

	// Sampling params and appended turns must not: the key has to stay
	// stable across a conversation so multi-turn replays keep homing to
	// the same replica.
	perturbed := bigChatBody()
	perturbed["temperature"] = 0.9
	perturbed["max_tokens"] = 123
	perturbed["messages"] = append(perturbed["messages"].([]any),
		map[string]any{"role": "assistant", "content": "answer"},
		map[string]any{"role": "user", "content": "follow-up"},
	)
	got := extract(t, perturbed, "/v1/chat/completions", testSalt)
	if got.Key != base {
		t.Error("sampling params and appended turns must not change the key")
	}
	if got.PromptBytes <= 6000 {
		t.Errorf("PromptBytes = %d, must include appended turns", got.PromptBytes)
	}
}

// TestExtractRequestDeterminism re-decodes the same JSON (fresh map iteration
// order) and expects an identical key.
func TestExtractRequestDeterminism(t *testing.T) {
	raw := `{"model":"m","tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"a":1,"b":2,"c":3,"d":4,"e":5}}}],"messages":[{"role":"system","content":"` + strings.Repeat("s", 5000) + `"},{"role":"user","content":"hello"}]}`
	var keys []Key
	for range 5 {
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, extract(t, body, "/v1/chat/completions", testSalt).Key)
	}
	for _, k := range keys[1:] {
		if k != keys[0] {
			t.Fatal("same JSON must derive the same key across decodes")
		}
	}
}

func TestExtractRequestEndpointShapes(t *testing.T) {
	long := strings.Repeat("x", 5000)

	responses := map[string]any{"instructions": "sys", "input": long}
	if req := extract(t, responses, "/v1/responses", testSalt); req.Outcome != OutcomeKeyed {
		t.Errorf("responses string input: outcome = %s, want keyed", req.Outcome)
	}

	responsesList := map[string]any{
		"instructions": "sys",
		"input": []any{
			map[string]any{"role": "user", "content": long},
		},
	}
	if req := extract(t, responsesList, "/v1/responses", testSalt); req.Outcome != OutcomeKeyed {
		t.Errorf("responses list input: outcome = %s, want keyed", req.Outcome)
	}

	completions := map[string]any{"prompt": long}
	if req := extract(t, completions, "/v1/completions", testSalt); req.Outcome != OutcomeKeyed {
		t.Errorf("completions: outcome = %s, want keyed", req.Outcome)
	}

	// The instructions field must feed the key on /v1/responses.
	a := extract(t, map[string]any{"instructions": "one", "input": long}, "/v1/responses", testSalt).Key
	b := extract(t, map[string]any{"instructions": "two", "input": long}, "/v1/responses", testSalt).Key
	if a == b {
		t.Error("instructions must change the key on /v1/responses")
	}
}

// TestExtractRequestWindowCap verifies the 1KB prefix window: divergence
// beyond it must not move the key, divergence inside it must.
func TestExtractRequestWindowCap(t *testing.T) {
	prefix := strings.Repeat("p", 5000) // clears the 4KB floor, exceeds the 1KB window
	a := extract(t, chatBody(prefix+"AAAA", "user"), "/v1/chat/completions", testSalt).Key
	b := extract(t, chatBody(prefix+"BBBB", "user"), "/v1/chat/completions", testSalt).Key
	if a != b {
		t.Error("divergence beyond the window cap must not change the key")
	}

	c := extract(t, chatBody("A"+prefix, "user"), "/v1/chat/completions", testSalt).Key
	d := extract(t, chatBody("B"+prefix, "user"), "/v1/chat/completions", testSalt).Key
	if c == d {
		t.Error("divergence inside the window must change the key")
	}

	// Total field length beyond the window must not move the key either: a
	// growing prompt with a stable prefix keeps its key.
	g := extract(t, chatBody("sys", prefix), "/v1/chat/completions", testSalt).Key
	h := extract(t, chatBody("sys", prefix+strings.Repeat("q", 1000)), "/v1/chat/completions", testSalt).Key
	if g != h {
		t.Error("field length beyond the window must not change the key")
	}
	i := extract(t, map[string]any{"prompt": prefix}, "/v1/completions", testSalt).Key
	j := extract(t, map[string]any{"prompt": prefix + strings.Repeat("q", 1000)}, "/v1/completions", testSalt).Key
	if i != j {
		t.Error("growing completions prompt with a stable prefix must keep its key")
	}
}

// TestFlattenLeafBounded pins the flattener's cost bound: a multi-megabyte
// leaf must cost at most the window, never a full copy.
func TestFlattenLeafBounded(t *testing.T) {
	out := flattenValue(strings.Repeat("x", 8<<20), prefixWindowCap)
	if len(out) > prefixWindowCap {
		t.Fatalf("flattened %d bytes, cap %d", len(out), prefixWindowCap)
	}
	if cap(out) > 4*prefixWindowCap {
		t.Fatalf("flatten buffer capacity %d betrays a full-leaf copy", cap(out))
	}
}

// TestExtractRequestMultimodalBounded exercises structured (multimodal)
// content: parts beyond the window cap must not affect the key, and
// PromptBytes must still count them.
func TestExtractRequestMultimodalBounded(t *testing.T) {
	multimodal := func(payload string) map[string]any {
		return map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": strings.Repeat("s", 5000)},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "describe this"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": payload}},
				}},
			},
		}
	}
	small := multimodal("data:image/png;base64," + strings.Repeat("A", 100))
	big := multimodal("data:image/png;base64," + strings.Repeat("B", 1<<20))

	reqSmall := extract(t, small, "/v1/chat/completions", testSalt)
	reqBig := extract(t, big, "/v1/chat/completions", testSalt)
	if reqSmall.Key != reqBig.Key {
		t.Error("content beyond the window cap must not change the key")
	}
	if reqBig.PromptBytes < 1<<20 {
		t.Errorf("PromptBytes = %d, must count the full multimodal payload", reqBig.PromptBytes)
	}
}

// TestExtractRequestMalformedShapes feeds hostile/degenerate bodies; nothing
// may panic out and outcomes must stay sensible.
func TestExtractRequestMalformedShapes(t *testing.T) {
	bodies := []map[string]any{
		nil,
		{"messages": "not a list"},
		{"messages": []any{"not a map", 42, nil}},
		{"messages": []any{map[string]any{"role": 7, "content": []any{nil}}}},
		{"tools": map[string]any{"weird": "shape"}, "prompt_cache_key": 42},
		{"input": []any{map[string]any{"role": "user"}}, "instructions": 9.5},
	}
	for i, body := range bodies {
		req := ExtractRequest(body, "/v1/chat/completions", testSalt, defaultSettings())
		if req.Outcome == OutcomeError {
			t.Errorf("body %d: unexpected error outcome", i)
		}
		if req.Outcome == OutcomeKeyed && req.Key == (Key{}) {
			t.Errorf("body %d: keyed with zero key", i)
		}
	}
}

// TestDeriveKeyPinned pins the key construction two ways: against a
// hand-framed reference for a case simple enough to build byte-by-byte
// (catching drift in the flattening or Sum wiring), and against a golden
// value (catching any change at all — changing the construction re-homes
// every key once routing acts, so it must be deliberate).
func TestDeriveKeyPinned(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys prompt"},
			map[string]any{"role": "user", "content": "user prompt"},
		},
		"prompt_cache_key": "group-1",
	}
	got := ExtractRequest(body, "/v1/chat/completions", testSalt, Settings{Mode: ModeShadow, MinPromptBytes: 1, Retention: DefaultRetention, SplitThresholdRPM: DefaultSplitThresholdRPM})
	if got.Outcome != OutcomeKeyed {
		t.Fatalf("outcome = %s, want keyed", got.Outcome)
	}

	// Hand-framed reference: string contents flatten to lp(content); the
	// window fields are Sum'd as separate lp() fields after the routing
	// secret (empty here: none installed), salt, and prompt_cache_key,
	// under the routing-key domain tag.
	lp := func(s string) string {
		n := len(s)
		return string([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}) + s
	}
	want := Key(cachesalt.Sum(KeyDomainTag, "", testSalt, "group-1", "", lp("sys prompt"), lp("user prompt")))
	if got.Key != want {
		t.Fatalf("key = %x, want hand-framed %x", got.Key, want)
	}

	const golden = "f3f6287264aaf4d7fe1a8ded41fdf7e78b7ebac31ebe0e15fc029599ccd6b4e4"
	if h := hex.EncodeToString(got.Key[:]); h != golden {
		t.Fatalf("key = %s, want golden %s (changing the construction re-homes every key)", h, golden)
	}
}

// TestDeriveKeyPinnedSecret pins the construction with a routing secret
// installed the same two ways: hand-framed reference and a golden from an
// independent implementation. Distinct secrets must derive distinct keys.
func TestDeriveKeyPinnedSecret(t *testing.T) {
	const secret = "test-routing-secret-0123456789abcdef"
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys prompt"},
			map[string]any{"role": "user", "content": "user prompt"},
		},
		"prompt_cache_key": "group-1",
	}
	settings := Settings{Mode: ModeShadow, MinPromptBytes: 1, Retention: DefaultRetention, SplitThresholdRPM: DefaultSplitThresholdRPM, Secret: secret}
	got := ExtractRequest(body, "/v1/chat/completions", testSalt, settings)
	if got.Outcome != OutcomeKeyed {
		t.Fatalf("outcome = %s, want keyed", got.Outcome)
	}

	lp := func(s string) string {
		n := len(s)
		return string([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}) + s
	}
	want := Key(cachesalt.Sum(KeyDomainTag, secret, testSalt, "group-1", "", lp("sys prompt"), lp("user prompt")))
	if got.Key != want {
		t.Fatalf("key = %x, want hand-framed %x", got.Key, want)
	}

	// Golden from an independent implementation (Python hashlib):
	// Sum(tag, secret, salt, "group-1", "", lp("sys prompt"),
	// lp("user prompt")).
	const golden = "6e6be0988ec76eb2e5d4c4b23ae0a4cecadcf4fbd27911570e164b894b6bf0a3"
	if h := hex.EncodeToString(got.Key[:]); h != golden {
		t.Fatalf("key = %s, want golden %s (changing the construction re-homes every key)", h, golden)
	}

	other := settings
	other.Secret = "other-routing-secret-0123456789abcdef"
	if r := ExtractRequest(body, "/v1/chat/completions", testSalt, other); r.Key == got.Key {
		t.Fatal("different secrets must derive different keys")
	}
}

// TestSettingsFromSecret verifies SettingsFrom resolves the installed
// process-wide secret for configured and unconfigured models alike, and
// that clearing it restores unkeyed derivation.
func TestSettingsFromSecret(t *testing.T) {
	const secret = "test-routing-secret-0123456789abcdef"
	SetSecret(secret)
	t.Cleanup(func() { SetSecret("") })

	if s := SettingsFrom(nil); s.Secret != secret {
		t.Fatalf("nil config must resolve the secret, got %q", s.Secret)
	}
	if s := SettingsFrom(&config.CacheRouteConfig{Mode: "enforced"}); s.Secret != secret {
		t.Fatalf("configured model must resolve the secret, got %q", s.Secret)
	}

	SetSecret("")
	if s := SettingsFrom(nil); s.Secret != "" {
		t.Fatalf("cleared secret must resolve empty, got %q", s.Secret)
	}
}

func TestSettingsFrom(t *testing.T) {
	def := SettingsFrom(nil)
	if def.Mode != ModeOff || def.Retention != DefaultRetention || def.MinPromptBytes != DefaultMinPromptBytes || def.SplitThresholdRPM != DefaultSplitThresholdRPM {
		t.Fatalf("nil config must resolve to off + defaults, got %+v", def)
	}

	s := SettingsFrom(&config.CacheRouteConfig{
		Mode:                   "shadow",
		RetentionWindowMinutes: 5,
		MinPromptBytes:         1024,
		SplitThresholdRPM:      30,
	})
	if s.Mode != ModeShadow || s.Retention.Minutes() != 5 || s.MinPromptBytes != 1024 || s.SplitThresholdRPM != 30 {
		t.Fatalf("explicit config not applied: %+v", s)
	}

	if s := SettingsFrom(&config.CacheRouteConfig{Mode: "enforced"}); s.Mode != ModeEnforced {
		t.Fatalf("enforced must resolve to enforced, got %s", s.Mode)
	}
	if s := SettingsFrom(&config.CacheRouteConfig{Mode: "bogus"}); s.Mode != ModeOff {
		t.Fatalf("unknown mode must resolve to off, got %s", s.Mode)
	}
}
