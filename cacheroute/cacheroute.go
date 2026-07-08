// Package cacheroute implements cache-aware replica routing (design doc §5)
// in shadow mode: it derives a routing key from the parsed request body,
// ranks a model's replicas with rendezvous hashing, and measures — via
// aggregate Prometheus metrics only — what cache-aware routing would have
// done and whether it would have helped. It never influences which replica
// serves a request.
//
// The router runs inside an enclave with no log sink, so /metrics is the only
// telemetry channel out. All per-key state (the shadow table in shadow.go)
// stays in process memory; nothing exported can be tied to a single request,
// key, or user. Metric labels are closed enum sets — a routing key or any
// value derived from one must never appear as a label.
//
// Like cachesalt, key derivation is a pure function of the request and pinned
// by tests: changing the domain tag, framing, field order, or the window
// flattening re-homes every key (a fleet-wide cache cold-start once routing
// acts on keys), so treat the construction as frozen.
package cacheroute

import (
	"slices"
	"strconv"
	"time"

	"github.com/tinfoilsh/confidential-model-router/cachesalt"
	"github.com/tinfoilsh/confidential-model-router/config"
)

// KeyDomainTag namespaces routing-key derivation. Like cachesalt's tags it is
// hashed unframed, so it must be a fixed constant and no tag anywhere in the
// router may be a prefix of another.
const KeyDomainTag = "tinfoil/route-key/v1"

// prefixWindowCap bounds the bytes of prompt prefix that feed the routing
// key: ~1KB ≈ 256 tokens, mirroring OpenAI's prefix-hash window (§5.1). The
// cap is a routing-hint tradeoff, not a correctness bound — cache_salt and
// the engine's byte-exact prefix cache remain the security and correctness
// boundaries.
const prefixWindowCap = 1024

// Defaults for the per-model Settings knobs (design doc §5.2, §5.4, and the
// implementation plan's Phase 4).
const (
	// DefaultRetention is W, the assumed KV-retention window: a replica
	// that served a key within W is assumed still warm for it.
	DefaultRetention = 10 * time.Minute
	// DefaultMinPromptBytes is the eligibility floor: prompts below it
	// aren't worth pinning (§5.2; ~1,000 tokens, OpenAI's cache floor).
	DefaultMinPromptBytes = 4096
	// DefaultSplitThresholdRPM is the per-key request rate that adds one
	// replica to a key's warm set (§5.4; OpenAI's figure is ~15 rpm).
	DefaultSplitThresholdRPM = 15
)

// Mode is the per-model rollout state of cache-aware routing.
type Mode string

const (
	// ModeOff disables the shadow pipeline entirely.
	ModeOff Mode = "off"
	// ModeShadow computes and meters routing decisions without acting.
	ModeShadow Mode = "shadow"
	// ModeEnforced acts on routing decisions. Not implemented until
	// Phase 5: resolveMode clamps it to ModeShadow so an early config
	// flip cannot silently claim behavior the build doesn't have.
	ModeEnforced Mode = "enforced"
)

// resolveMode maps a raw config mode string to the mode this build actually
// runs. It reports clamped=true when the config asked for enforcement, which
// this Phase 4 build downgrades to shadow; the mode metric must report the
// effective mode so dashboards never lie. Unknown values resolve to off.
func resolveMode(raw string) (mode Mode, clamped bool) {
	switch Mode(raw) {
	case ModeShadow:
		return ModeShadow, false
	case ModeEnforced:
		return ModeShadow, true
	default:
		return ModeOff, false
	}
}

// Settings are the resolved per-model cache-route knobs.
type Settings struct {
	Mode              Mode
	Retention         time.Duration // W: how long a replica is assumed warm for a key
	MinPromptBytes    int           // eligibility floor on whole-prompt bytes
	SplitThresholdRPM float64       // per-key rpm per replica of warm set
}

// SettingsFrom resolves a config block (nil means unconfigured) to concrete
// settings, applying package defaults for zero fields.
func SettingsFrom(c *config.CacheRouteConfig) Settings {
	s := Settings{
		Mode:              ModeOff,
		Retention:         DefaultRetention,
		MinPromptBytes:    DefaultMinPromptBytes,
		SplitThresholdRPM: DefaultSplitThresholdRPM,
	}
	if c == nil {
		return s
	}
	s.Mode, _ = resolveMode(c.Mode)
	if c.RetentionWindowMinutes > 0 {
		s.Retention = time.Duration(c.RetentionWindowMinutes) * time.Minute
	}
	if c.MinPromptBytes > 0 {
		s.MinPromptBytes = c.MinPromptBytes
	}
	if c.SplitThresholdRPM > 0 {
		s.SplitThresholdRPM = c.SplitThresholdRPM
	}
	return s
}

// Outcome is the eligibility-funnel classification of a request, used as the
// outcome label of router_cache_route_requests_total. Values form a closed
// set; dashboards depend on them.
type Outcome string

const (
	// OutcomeKeyed means a routing key was derived and the full shadow
	// pipeline ran.
	OutcomeKeyed Outcome = "keyed"
	// OutcomeNoSalt means the request carries no injected cache_salt. The
	// salt is a routing-key component (§5.1) and the rollout invariant is
	// no salt ⇒ no cache-aware routing.
	OutcomeNoSalt Outcome = "no_salt"
	// OutcomeNoPrefix means no prompt fields could be extracted, so there
	// is nothing to key on.
	OutcomeNoPrefix Outcome = "no_prefix"
	// OutcomeBelowFloor means the prompt is under the eligibility floor
	// and carries no prompt_cache_key (§5.2: short prompts aren't worth
	// pinning).
	OutcomeBelowFloor Outcome = "below_floor"
	// OutcomePoolTooSmall means the model has fewer than two configured
	// replicas, so routing can't move anything.
	OutcomePoolTooSmall Outcome = "pool_too_small"
	// OutcomeError means the shadow path panicked and was recovered. Must
	// stay ≈ 0; it exists to prove shadow never fails a request.
	OutcomeError Outcome = "error"
)

// Key is a derived routing key: a domain-tagged SHA-256 over the cache salt,
// client prompt_cache_key, and prompt prefix window. Internal to the router,
// never sent downstream, never logged, never a metric label.
type Key [32]byte

// Request is the shadow pipeline's view of one request, produced by
// ExtractRequest at body-parse time and consumed by Shadow.Observe after the
// production selector has picked a replica.
type Request struct {
	// Outcome is the funnel classification. Key is only meaningful when
	// Outcome == OutcomeKeyed.
	Outcome Outcome
	Key     Key
	// PromptBytes approximates the whole-prompt size (tools + all message
	// contents), the prefill-cost weight for the byte-weighted reuse
	// metrics and the eligibility floor input.
	PromptBytes int

	elapsed time.Duration // extraction cost, folded into compute_seconds by Observe
}

// ExtractRequest classifies a parsed OpenAI-compatible request body and, when
// eligible, derives its routing key. salt is the encoded cache_salt string
// the router injected ("" when none). It never fails: any panic is recovered
// into OutcomeError, and malformed shapes simply classify as ineligible.
//
// Call it only on endpoints that support cache_salt and only after the body
// is final (post file-input rewriting, post salt injection), so the window is
// built from what the engine will actually see.
func ExtractRequest(body map[string]any, path, salt string, s Settings) (req *Request) {
	start := time.Now()
	req = &Request{Outcome: OutcomeError}
	defer func() {
		if r := recover(); r != nil {
			req.Outcome = OutcomeError
		}
		req.elapsed = time.Since(start)
	}()

	promptCacheKey, _ := body["prompt_cache_key"].(string)
	tools, system, user := promptFields(body, path)
	req.PromptBytes = promptLen(body, path, tools)

	switch {
	case salt == "":
		req.Outcome = OutcomeNoSalt
	case tools == nil && system == nil && user == nil && promptCacheKey == "":
		req.Outcome = OutcomeNoPrefix
	case req.PromptBytes < s.MinPromptBytes && promptCacheKey == "":
		// A request carrying prompt_cache_key is always eligible: an
		// explicit client grouping is honored as stated (§5.2).
		req.Outcome = OutcomeBelowFloor
	default:
		req.Outcome = OutcomeKeyed
		req.Key = deriveKey(salt, promptCacheKey, tools, system, user)
	}
	return req
}

// deriveKey computes the routing key (§5.1):
//
//	routing_key = hash(cache_salt ‖ prompt_cache_key ‖ prefix_window)
//	prefix_window = lp(tools) ‖ lp(system) ‖ lp(first user message), 1KB cap
//
// using cachesalt.Sum's domain-tagged, length-prefixed framing (§6), with the
// three window fields passed as separate lp()-framed fields and the cap
// applied across their total. The salt input is the encoded 43-char salt
// string the router injects, exactly as the design doc pins it. Missing
// fields contribute lp("") via their nil flattening, keeping the encoding
// deterministic.
func deriveKey(salt, promptCacheKey string, tools, system, user any) Key {
	budget := prefixWindowCap
	toolsB := flattenValue(tools, budget)
	budget -= len(toolsB)
	systemB := flattenValue(system, budget)
	budget -= len(systemB)
	userB := flattenValue(user, budget)
	return Key(cachesalt.Sum(KeyDomainTag, salt, promptCacheKey, string(toolsB), string(systemB), string(userB)))
}

// promptFields pulls the three prefix-window fields out of the parsed body in
// prompt-render order (tools → system → first user message), per endpoint
// shape. These are the extracted fields that become the engine's cached
// prefix — never raw body bytes, whose leading content is sampling params and
// marshaler field order (§5.1's mis-keying argument). A missing field is nil.
func promptFields(body map[string]any, path string) (tools, system, user any) {
	tools = body["tools"]

	switch path {
	case "/v1/responses":
		// The Responses API carries the system prompt in `instructions`
		// and the conversation in `input` (a string, or a list of
		// role-tagged items).
		system = body["instructions"]
		switch input := body["input"].(type) {
		case string:
			user = input
		case []any:
			user = firstMessageContent(input, "user")
		}
	case "/v1/completions":
		user = body["prompt"]
	default: // /v1/chat/completions and anything chat-shaped
		if messages, ok := body["messages"].([]any); ok {
			// OpenAI-compatible clients use either role for the
			// system prompt; take whichever appears first.
			system = firstMessageContent(messages, "system", "developer")
			user = firstMessageContent(messages, "user")
		}
	}
	return tools, system, user
}

// firstMessageContent returns the content of the first message whose role is
// one of roles, or nil.
func firstMessageContent(messages []any, roles ...string) any {
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		for _, want := range roles {
			if role == want {
				return msg["content"]
			}
		}
	}
	return nil
}

// promptLen approximates §5.2's prompt_len — the total byte length of tools
// plus all message contents (the parsed fields, not the raw body) — by
// summing the string content reachable from those fields. It is the
// eligibility-floor input and the prefill-cost weight; a size proxy, not an
// exact token count.
func promptLen(body map[string]any, path string, tools any) int {
	n := valueLen(tools)
	switch path {
	case "/v1/responses":
		n += valueLen(body["instructions"])
		n += messageContentLen(body["input"])
	case "/v1/completions":
		n += valueLen(body["prompt"])
	default:
		n += messageContentLen(body["messages"])
	}
	return n
}

// messageContentLen sums valueLen over the content field of each message in
// a messages/input list. Non-list shapes (e.g. a string input) are measured
// directly.
func messageContentLen(v any) int {
	list, ok := v.([]any)
	if !ok {
		return valueLen(v)
	}
	n := 0
	for _, m := range list {
		if msg, ok := m.(map[string]any); ok {
			n += valueLen(msg["content"])
		}
	}
	return n
}

// valueLen is the length counterpart of flattenValue: the total bytes its
// string leaves would contribute, computed without building anything. Only
// string lengths count — structure, numbers, and bools are size noise for a
// floor threshold.
func valueLen(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		n := 0
		for _, e := range x {
			n += valueLen(e)
		}
		return n
	case map[string]any:
		n := 0
		for _, e := range x {
			n += valueLen(e)
		}
		return n
	default:
		return 0
	}
}

// flattenValue renders a parsed JSON value into deterministic bytes for the
// prefix window, spending at most budget bytes. It exists instead of
// json.Marshal for one reason: boundedness. A multimodal first user message
// can carry megabytes of inline image data, and marshaling it whole to keep
// ≤1KB would put an unbounded allocation on every request the shadow
// observes. Traversal stops the moment the budget is spent.
//
// Determinism is what the routing key needs (every router must derive the
// same bytes from the same parsed value), and three choices provide it: map
// keys are visited in sorted order, every field is lp()-framed so boundaries
// can't shift (§6), and scalars render via strconv. The encoding is pinned by
// test vectors; changing it re-homes every key.
func flattenValue(v any, budget int) []byte {
	if budget <= 0 {
		return nil
	}
	buf := make([]byte, 0, min(budget, 256))
	buf = appendFlattened(buf, v, budget)
	return buf
}

// appendFlattened appends v's flattening to buf, never growing it past
// budget. Strings are lp-framed; containers frame each element (and each
// sorted map key) so distinct structures can't collide. The final append may
// overshoot mid-token; the tail is truncated to the budget, which is safe
// because a truncated window is still deterministic — both routers truncate
// identically.
func appendFlattened(buf []byte, v any, budget int) []byte {
	if len(buf) >= budget {
		return buf[:budget]
	}
	switch x := v.(type) {
	case nil:
		// Contributes nothing: a missing field is lp("") at the Sum
		// layer.
	case string:
		buf = appendFramed(buf, x)
	case bool:
		buf = appendFramed(buf, strconv.FormatBool(x))
	case float64:
		buf = appendFramed(buf, strconv.FormatFloat(x, 'g', -1, 64))
	case []any:
		for _, e := range x {
			if len(buf) >= budget {
				break
			}
			buf = appendFlattened(buf, e, budget)
		}
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if len(buf) >= budget {
				break
			}
			buf = appendFramed(buf, k)
			buf = appendFlattened(buf, x[k], budget)
		}
	default:
		// json.Number and exotic shapes: render via their string form
		// when available; otherwise contribute nothing.
		if s, ok := v.(interface{ String() string }); ok {
			buf = appendFramed(buf, s.String())
		}
	}
	if len(buf) > budget {
		buf = buf[:budget]
	}
	return buf
}

// appendFramed appends lp(s) = uint32_be(len(s)) ‖ s.
func appendFramed(buf []byte, s string) []byte {
	n := uint32(len(s))
	buf = append(buf, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(buf, s...)
}

// sortedKeys returns m's keys in sorted order for deterministic traversal.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
