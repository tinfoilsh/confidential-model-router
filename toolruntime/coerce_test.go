package toolruntime

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaFromJSON compiles a tiny JSON Schema literal into a *jsonschema.Schema.
// The tests use this instead of hand-building struct literals because the
// router's real call path also unmarshals schemas that arrived over the wire.
func schemaFromJSON(t *testing.T, raw string) *jsonschema.Schema {
	t.Helper()
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("schemaFromJSON: %v", err)
	}
	return &s
}

// searchSchemaJSON mirrors the real search tool input schema closely enough
// to pin the fields that regressed with llama ("max_results",
// "max_content_chars", "allowed_domains", and an enum-backed
// "content_mode"). Keeping it here rather than importing the websearch
// server's schema keeps the toolruntime tests hermetic.
const searchSchemaJSON = `{
    "type": "object",
    "properties": {
        "query":              {"type": "string"},
        "max_results":        {"type": "integer", "minimum": 1, "maximum": 50},
        "max_content_chars":  {"type": "integer"},
        "content_mode":       {"type": "string", "enum": ["highlights","text"]},
        "allowed_domains":    {"type": "array", "items": {"type": "string"}},
        "max_age_hours":      {"type": "integer"},
        "enable_pii_check":   {"type": "boolean"},
        "user_location": {
            "type": "object",
            "properties": {
                "approximate": {
                    "type": "object",
                    "properties": {
                        "city":    {"type": "string"},
                        "country": {"type": "string"}
                    }
                }
            }
        }
    }
}`

func searchSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	return map[string]*jsonschema.Schema{
		"search": schemaFromJSON(t, searchSchemaJSON),
	}
}

// TestCoerce_StringIntegerFieldsBecomeInt64 reproduces the observed
// llama3-3-70b failure where "max_results":"1" and
// "max_content_chars":"700" were rejected server-side. After coercion both
// must be int64 so the downstream validator accepts them. This is the
// single test that proves the production bug stops happening.
func TestCoerce_StringIntegerFieldsBecomeInt64(t *testing.T) {
	args := map[string]any{
		"query":             "Neighborhood Cat Gazette",
		"max_results":       "1",
		"max_content_chars": "700",
	}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if got, ok := args["max_results"].(int64); !ok || got != 1 {
		t.Fatalf("max_results: got %[1]T(%[1]v), want int64(1)", args["max_results"])
	}
	if got, ok := args["max_content_chars"].(int64); !ok || got != 700 {
		t.Fatalf("max_content_chars: got %[1]T(%[1]v), want int64(700)", args["max_content_chars"])
	}
	if args["query"] != "Neighborhood Cat Gazette" {
		t.Fatalf("query unexpectedly mutated: %v", args["query"])
	}
}

// TestCoerce_BogusIntegerLeftUntouched pins the "garbage in, garbage out"
// contract: when a string cannot possibly be an integer the router should
// pass it through unmodified so the server's validator still produces the
// canonical error. Silently rewriting bad values would hide user bugs.
func TestCoerce_BogusIntegerLeftUntouched(t *testing.T) {
	args := map[string]any{"max_results": "not-a-number"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if args["max_results"] != "not-a-number" {
		t.Fatalf("bogus integer mutated to %v", args["max_results"])
	}
}

// TestCoerce_IntegerOutOfRangeLeftUntouched ensures min/max constraints
// from the schema are honored during coercion. Rewriting a value that
// then fails on range would just trade one validator error for another
// and lose the original string for debugging.
func TestCoerce_IntegerOutOfRangeLeftUntouched(t *testing.T) {
	args := map[string]any{"max_results": "9999"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if args["max_results"] != "9999" {
		t.Fatalf("out-of-range value coerced anyway: %v", args["max_results"])
	}
}

// TestCoerce_AlreadyCorrectTypeIsIdempotent guards against accidental
// mutation of arguments that already match the schema. Callers rely on
// the coercer being a no-op in the common case where the upstream model
// emitted the right types. int64 stays int64, strings stay strings, and
// valid enum values pass through untouched.
func TestCoerce_AlreadyCorrectTypeIsIdempotent(t *testing.T) {
	args := map[string]any{
		"query":        "cats",
		"max_results":  int64(5),
		"content_mode": "highlights",
	}
	snapshot := map[string]any{"query": "cats", "max_results": int64(5), "content_mode": "highlights"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if !reflect.DeepEqual(args, snapshot) {
		t.Fatalf("coercer mutated already-valid args: got %v, want %v", args, snapshot)
	}
}

// TestCoerce_BooleanStringBecomesBool exercises the boolean branch. Some
// upstream models emit "true"/"false" as JSON strings for boolean opt-ins
// like PII check. Those need to become real booleans before the call
// reaches the MCP server.
func TestCoerce_BooleanStringBecomesBool(t *testing.T) {
	args := map[string]any{"enable_pii_check": "true"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if args["enable_pii_check"] != true {
		t.Fatalf("enable_pii_check: got %v, want true", args["enable_pii_check"])
	}
}

// TestCoerce_ArrayFromJSONString covers models that encode lists as a
// JSON array literal instead of a native array, which was observed in
// the wild for allowed_domains.
func TestCoerce_ArrayFromJSONString(t *testing.T) {
	args := map[string]any{"allowed_domains": `["python.org","docs.python.org"]`}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	got, ok := args["allowed_domains"].([]any)
	if !ok {
		t.Fatalf("allowed_domains: got %[1]T(%[1]v), want []any", args["allowed_domains"])
	}
	want := []any{"python.org", "docs.python.org"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_domains: got %v, want %v", got, want)
	}
}

// TestCoerce_ArrayFromCSVString covers the fallback CSV path. The
// precise format is not specified by JSON Schema; treating a comma
// list as a pragmatic array recovery keeps otherwise-valid calls from
// failing in a chat UI.
func TestCoerce_ArrayFromCSVString(t *testing.T) {
	args := map[string]any{"allowed_domains": "python.org, docs.python.org"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	got, ok := args["allowed_domains"].([]any)
	if !ok {
		t.Fatalf("allowed_domains: got %[1]T(%[1]v), want []any", args["allowed_domains"])
	}
	want := []any{"python.org", "docs.python.org"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_domains: got %v, want %v", got, want)
	}
}

// TestCoerce_SingleTokenNotSplitAsCSV proves the "python.org" case does
// not get mangled into an array. The CSV fallback only activates when
// there are two or more non-empty comma-separated fields so a legitimate
// single-token string stays a string-typed array after coercion.
func TestCoerce_SingleTokenNotSplitAsCSV(t *testing.T) {
	args := map[string]any{"allowed_domains": "python.org"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if got := args["allowed_domains"]; got != "python.org" {
		t.Fatalf("single-token CSV mis-coerced to %[1]T(%[1]v)", got)
	}
}

// TestCoerce_EnumViolationLeftUntouched pins the enum-preservation rule:
// we never rewrite a value if the new value would not satisfy a declared
// enum. Returning the original string keeps the server's error message
// truthful and preserves the caller's debugging signal.
func TestCoerce_EnumViolationLeftUntouched(t *testing.T) {
	args := map[string]any{"content_mode": "raw"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if args["content_mode"] != "raw" {
		t.Fatalf("content_mode: got %v, want unchanged", args["content_mode"])
	}
}

// TestCoerce_NestedObjectFieldsCoerced confirms the coercer recurses
// into declared sub-objects. A model that emits
// user_location.approximate.city as a numeric-looking string would not
// normally need rescue, but this test pins the recursion path so future
// changes can't quietly break it.
func TestCoerce_NestedObjectFieldsCoerced(t *testing.T) {
	args := map[string]any{
		"user_location": map[string]any{
			"approximate": map[string]any{
				"city":    "London",
				"country": "GB",
			},
		},
	}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	approx := args["user_location"].(map[string]any)["approximate"].(map[string]any)
	if approx["city"] != "London" || approx["country"] != "GB" {
		t.Fatalf("nested object unexpectedly mutated: %v", approx)
	}
}

// TestCoerce_NestedObjectFromJSONStringExpands covers the case where an
// entire nested object arrives as a JSON-encoded string. After coercion
// the value should be a real map so the server schema validates it.
func TestCoerce_NestedObjectFromJSONStringExpands(t *testing.T) {
	args := map[string]any{
		"user_location": `{"approximate":{"city":"London","country":"GB"}}`,
	}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	loc, ok := args["user_location"].(map[string]any)
	if !ok {
		t.Fatalf("user_location: got %[1]T, want map[string]any", args["user_location"])
	}
	approx, ok := loc["approximate"].(map[string]any)
	if !ok {
		t.Fatalf("approximate: got %[1]T, want map[string]any", loc["approximate"])
	}
	if approx["city"] != "London" || approx["country"] != "GB" {
		t.Fatalf("expanded nested values wrong: %v", approx)
	}
}

// TestCoerce_FloatWholeNumberBecomesInt covers models that emit
// floating-point numbers even for integer-typed fields ("max_results": 5.0).
// Silently downcasting to int64 is safe because the fractional part is
// exactly zero.
func TestCoerce_FloatWholeNumberBecomesInt(t *testing.T) {
	args := map[string]any{"max_results": 5.0}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	// 5.0 already matches "number", but the schema says integer, so the
	// coercer should rewrite it to int64 for validator-friendliness.
	if got, ok := args["max_results"].(int64); !ok || got != 5 {
		t.Fatalf("max_results: got %[1]T(%[1]v), want int64(5)", args["max_results"])
	}
}

// TestCoerce_FloatOverflowLeftUntouched guards the coerceMaxIntFromFloat
// magnitude bound on the float branches. Go's spec leaves float-to-int64
// conversion undefined when the source exceeds int64 range, and Go 1
// implementations saturate silently (1e100 -> MaxInt64, +Inf -> MaxInt64,
// -Inf -> MinInt64). Rewriting those to saturated int64 would ship a
// wildly different value to the MCP server than the model intended, so
// the router must leave the argument in place and let the server's
// canonical validator respond.
func TestCoerce_FloatOverflowLeftUntouched(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"float64_1e100", 1e100},
		{"float64_neg_1e100", -1e100},
		{"float32_huge", float32(1e30)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"max_content_chars": tc.value}
			coerceArgumentsToSchema("search", args, searchSchemas(t))
			if _, converted := args["max_content_chars"].(int64); converted {
				t.Fatalf("overflow value silently coerced to int64: %v", args["max_content_chars"])
			}
			if !reflect.DeepEqual(args["max_content_chars"], tc.value) {
				t.Fatalf("value mutated: got %[1]T(%[1]v), want %[2]T(%[2]v)",
					args["max_content_chars"], tc.value)
			}
		})
	}
}

// TestCoerce_FloatNonFiniteLeftUntouched pins the NaN / +-Inf case. Even
// though encoding/json itself refuses to produce these, the argument map
// can also be populated by Go-native callers (coerce is a reusable helper
// and quirk repairs hand back the same map shape). Converting NaN to 0
// or Inf to MaxInt64 would be a silent data-corruption bug.
func TestCoerce_FloatNonFiniteLeftUntouched(t *testing.T) {
	cases := []struct {
		name  string
		value float64
	}{
		{"nan", math.NaN()},
		{"pos_inf", math.Inf(1)},
		{"neg_inf", math.Inf(-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"max_content_chars": tc.value}
			coerceArgumentsToSchema("search", args, searchSchemas(t))
			if _, converted := args["max_content_chars"].(int64); converted {
				t.Fatalf("non-finite value silently coerced to int64: %v", args["max_content_chars"])
			}
		})
	}
}

// TestCoerce_StringScientificOverflowLeftUntouched re-asserts that the
// pre-existing guard on the string branch still fires after the float
// helper was factored out; the two code paths must stay symmetric.
func TestCoerce_StringScientificOverflowLeftUntouched(t *testing.T) {
	args := map[string]any{"max_content_chars": "1e100"}
	coerceArgumentsToSchema("search", args, searchSchemas(t))
	if args["max_content_chars"] != "1e100" {
		t.Fatalf("string overflow mutated to %v", args["max_content_chars"])
	}
}

// TestCoerce_UnknownToolIsNoop ensures the coercer stays silent when a
// tool the model invoked isn't in the schema lookup (a tool added by the
// client, for example).
func TestCoerce_UnknownToolIsNoop(t *testing.T) {
	args := map[string]any{"max_results": "1"}
	coerceArgumentsToSchema("custom_tool", args, searchSchemas(t))
	if args["max_results"] != "1" {
		t.Fatalf("unknown tool coercion touched args: %v", args["max_results"])
	}
}

// TestCoerce_NilInputsAreNoop is a paranoia check for the degenerate
// inputs the caller could plausibly hand the function during teardown or
// when a tool schema isn't available yet.
func TestCoerce_NilInputsAreNoop(t *testing.T) {
	coerceArgumentsToSchema("search", nil, nil)
	coerceArgumentsToSchema("search", map[string]any{}, nil)
	coerceArgumentsToSchema("search", nil, searchSchemas(t))
}

// TestSchemaLookup_FromMCPTool ensures we rehydrate the map[string]any
// InputSchema that arrives from the MCP client side into a real
// *jsonschema.Schema we can walk.
func TestSchemaLookup_FromMCPTool(t *testing.T) {
	inputSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"n": map[string]any{"type": "integer"},
		},
	}
	tools := []*mcp.Tool{{Name: "demo", InputSchema: inputSchema}}
	got := schemaLookup(tools)
	if got["demo"] == nil {
		t.Fatalf("schemaLookup missing demo tool")
	}
	if got["demo"].Properties["n"].Type != "integer" {
		t.Fatalf("expected n:integer, got %+v", got["demo"].Properties["n"])
	}
}

// TestHumanizeToolArgError_PullsFieldAndTypes verifies the model-facing
// wrapper extracts the salient path and type mismatch from the raw
// validator blob we observed in production logs.
func TestHumanizeToolArgError_PullsFieldAndTypes(t *testing.T) {
	rawErr := errors.New(`validating "arguments": validating root: validating /properties/max_results: type: 1 has type "string", want "integer"`)
	got := humanizeToolArgError("search", rawErr, map[string]any{"max_results": "1"})
	if got == "" {
		t.Fatal("humanizeToolArgError returned empty string")
	}
	for _, needle := range []string{`"max_results"`, "integer", "string", "search"} {
		if !contains(got, needle) {
			t.Fatalf("humanized error missing %q: %q", needle, got)
		}
	}
}

// TestHumanizeToolArgError_FallsBackOnUnknownShape guarantees the
// wrapper does not throw away information when the upstream error does
// not match the canonical format the extractor is looking for.
func TestHumanizeToolArgError_FallsBackOnUnknownShape(t *testing.T) {
	rawErr := errors.New("some random transport error")
	got := humanizeToolArgError("search", rawErr, map[string]any{})
	if !contains(got, "some random transport error") {
		t.Fatalf("fallback dropped original error text: %q", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
