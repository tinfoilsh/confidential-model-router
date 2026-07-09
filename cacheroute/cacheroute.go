// Package cacheroute implements cache-aware replica routing: it derives a
// routing key from the parsed request body, ranks a model's replicas with
// rendezvous hashing, and — per pool — either only measures what routing
// would have done (shadow) or drives replica selection (enforced, via
// Shadow.Decide feeding the manager's selectors).
//
// The router's only telemetry channel is Prometheus, so everything the
// pipeline learns is exported as aggregate metrics over closed label sets.
// Per-key state stays in process memory; a routing key, or anything derived
// from one, must never appear in a label or log line.
package cacheroute

import (
	"slices"
	"strconv"
	"time"

	"github.com/tinfoilsh/confidential-model-router/cachesalt"
	"github.com/tinfoilsh/confidential-model-router/config"
)

// KeyDomainTag namespaces routing-key derivation. Tags are hashed unframed,
// so no tag in the router may be a prefix of another.
const KeyDomainTag = "tinfoil/route-key/v1"

// prefixWindowCap bounds how much prompt prefix feeds the routing key
// (~256 tokens). A routing hint only: the engine's byte-exact prefix cache
// remains the correctness boundary.
const prefixWindowCap = 1024

const (
	// DefaultRetention is the assumed KV-retention window: a replica that
	// served a key within it is assumed still warm for that key.
	DefaultRetention = 10 * time.Minute
	// DefaultMinPromptBytes is the eligibility floor: shorter prompts
	// aren't worth pinning to a replica.
	DefaultMinPromptBytes = 4096
	// DefaultSplitThresholdRPM is the per-key request rate that adds one
	// replica to a hot key's warm set (OpenAI splits at ~15 rpm).
	DefaultSplitThresholdRPM = 15
)

// Mode is a model's cache-route rollout state.
type Mode string

const (
	// ModeOff disables the pipeline entirely.
	ModeOff Mode = "off"
	// ModeShadow computes and meters routing decisions without acting.
	ModeShadow Mode = "shadow"
	// ModeEnforced acts on routing decisions: keyed requests are served
	// by their cache-aware pick instead of a random replica.
	ModeEnforced Mode = "enforced"
)

// resolveMode maps a raw config string to a mode. Unknown values resolve to
// off.
func resolveMode(raw string) Mode {
	switch Mode(raw) {
	case ModeShadow:
		return ModeShadow
	case ModeEnforced:
		return ModeEnforced
	default:
		return ModeOff
	}
}

// Settings are a model's resolved cache-route knobs.
type Settings struct {
	Mode              Mode
	Retention         time.Duration // how long a replica stays warm for a key
	MinPromptBytes    int           // eligibility floor on whole-prompt bytes
	SplitThresholdRPM float64       // per-key rpm per replica of warm set
}

// SettingsFrom resolves a config block (nil means unconfigured), applying
// defaults for zero fields.
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
	s.Mode = resolveMode(c.Mode)
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

// Outcome classifies a request for the eligibility funnel
// (router_cache_route_requests_total). Closed set; dashboards depend on it.
type Outcome string

const (
	// OutcomeKeyed: a routing key was derived and the full pipeline ran.
	OutcomeKeyed Outcome = "keyed"
	// OutcomeNoSalt: no injected cache_salt, which is a routing-key
	// component — no salt means no cache-aware routing.
	OutcomeNoSalt Outcome = "no_salt"
	// OutcomeNoPrefix: no prompt fields to key on.
	OutcomeNoPrefix Outcome = "no_prefix"
	// OutcomeBelowFloor: prompt under the floor with no prompt_cache_key.
	OutcomeBelowFloor Outcome = "below_floor"
	// OutcomePoolTooSmall: fewer than two configured replicas.
	OutcomePoolTooSmall Outcome = "pool_too_small"
	// OutcomeError: the shadow path panicked and was recovered. Must stay
	// ≈ 0.
	OutcomeError Outcome = "error"
)

// Key is a derived routing key. Internal to the router: never sent
// downstream, logged, or used as a metric label.
type Key [32]byte

// Request is the shadow pipeline's view of one request, produced by
// ExtractRequest at body-parse time and consumed by Shadow.Observe after
// replica selection.
type Request struct {
	// Outcome is the funnel classification; Key is meaningful only when
	// Outcome is OutcomeKeyed.
	Outcome Outcome
	Key     Key
	// PromptBytes approximates whole-prompt size (tools + all message
	// contents): the eligibility-floor input and the weight for the
	// byte-weighted reuse metrics.
	PromptBytes int

	elapsed time.Duration // extraction cost, folded into ComputeSeconds
}

// ExtractRequest classifies a parsed OpenAI-compatible body and, when
// eligible, derives its routing key. salt is the cache_salt string the
// router injected ("" when none). It never fails: panics are recovered into
// OutcomeError and malformed shapes classify as ineligible. Call it after
// the body is final (post file-input rewriting, post salt injection) so the
// window reflects what the engine will see.
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
		// An explicit prompt_cache_key makes a request eligible
		// regardless of size.
		req.Outcome = OutcomeBelowFloor
	default:
		req.Outcome = OutcomeKeyed
		req.Key = deriveKey(salt, promptCacheKey, tools, system, user)
	}
	return req
}

// deriveKey computes
//
//	Sum(tag, salt, prompt_cache_key, tools, system, first user message)
//
// with the three prompt fields flattened and capped to prefixWindowCap
// across their total. Sum length-prefixes every field, so boundaries can't
// shift. The construction is pinned by test vectors: changing any detail
// re-homes every key, a fleet-wide cache cold-start once routing acts.
func deriveKey(salt, promptCacheKey string, tools, system, user any) Key {
	budget := prefixWindowCap
	toolsB := flattenValue(tools, budget)
	budget -= len(toolsB)
	systemB := flattenValue(system, budget)
	budget -= len(systemB)
	userB := flattenValue(user, budget)
	return Key(cachesalt.Sum(KeyDomainTag, salt, promptCacheKey, string(toolsB), string(systemB), string(userB)))
}

// promptFields pulls the routing-relevant fields out of the parsed body in
// prompt-render order (tools → system → first user message), per endpoint
// shape. Keying on parsed fields rather than raw body bytes keeps volatile
// non-prompt content (sampling params, marshaler field order) out of the
// key. A missing field is nil.
func promptFields(body map[string]any, path string) (tools, system, user any) {
	tools = body["tools"]

	switch path {
	case "/v1/responses":
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
			// Clients use either role for the system prompt.
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

// promptLen approximates whole-prompt size — tools plus all message
// contents — by summing reachable string bytes. A size proxy for the floor
// threshold, not a token count.
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

// messageContentLen sums valueLen over each message's content field;
// non-list shapes are measured directly.
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

// valueLen sums the string bytes reachable from v without building
// anything.
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
// prefix window, spending at most budget. It replaces json.Marshal for one
// reason: boundedness — a multimodal message can carry megabytes of inline
// image data, and the walk must stop at the budget rather than marshal the
// whole value first.
func flattenValue(v any, budget int) []byte {
	if budget <= 0 {
		return nil
	}
	buf := make([]byte, 0, min(budget, 256))
	buf = appendFlattened(buf, v, budget)
	return buf
}

// appendFlattened appends v's flattening to buf, never growing past budget.
// Determinism is load-bearing: map keys visit in sorted order, every
// element is length-prefixed so boundaries can't shift, and scalars render
// via strconv.
func appendFlattened(buf []byte, v any, budget int) []byte {
	if len(buf) >= budget {
		return buf
	}
	switch x := v.(type) {
	case nil:
		// A missing field contributes nothing.
	case string:
		buf = appendFramed(buf, x, budget)
	case bool:
		buf = appendFramed(buf, strconv.FormatBool(x), budget)
	case float64:
		buf = appendFramed(buf, strconv.FormatFloat(x, 'g', -1, 64), budget)
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
			buf = appendFramed(buf, k, budget)
			buf = appendFlattened(buf, x[k], budget)
		}
	default:
		// json.Number renders via String(); anything else contributes
		// nothing.
		if s, ok := v.(interface{ String() string }); ok {
			buf = appendFramed(buf, s.String(), budget)
		}
	}
	return buf
}

// appendFramed appends lp(s) = uint32_be(len(s)) ‖ s, truncating s so the
// frame fits the budget. Truncating BEFORE framing is load-bearing twice:
// the header encodes the window-visible length, so bytes beyond the window
// can never influence the key (a growing prompt with a stable prefix must
// keep its key), and a multi-megabyte leaf costs at most the window, never a
// full copy.
func appendFramed(buf []byte, s string, budget int) []byte {
	room := budget - len(buf) - 4
	if room <= 0 {
		return buf
	}
	if len(s) > room {
		s = s[:room]
	}
	n := uint32(len(s))
	buf = append(buf, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(buf, s...)
}

// sortedKeys returns m's keys sorted, for deterministic traversal.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
