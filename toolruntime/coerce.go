package toolruntime

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool arguments emitted by upstream models routinely arrive with JSON types
// that do not match what the MCP server's InputSchema declares. Llama-family
// models, for instance, stringify every numeric argument ("max_results":"1"),
// which causes the server-side validator to reject the call. The model then
// sees a raw validator blob, cannot self-correct, and burns the tool budget
// retrying the same mistake. coerceArgumentsToSchema fixes this centrally by
// walking the tool's published schema and converting string-encoded primitives
// into the declared types before the call leaves the router. The policy is
// deliberately conservative: an argument is only rewritten when the new value
// clearly satisfies the schema; otherwise it is left untouched so the server's
// validator still produces its canonical error.

const (
	// coerceMaxIntFromFloat guards against silently downcasting
	// scientific-notation strings that happen to parse as valid floats
	// but overflow int64 (for example, "1e100"). We reject anything
	// whose magnitude exceeds this bound so the server still sees the
	// original string and responds with its canonical out-of-range error.
	coerceMaxIntFromFloat = float64(1 << 53)
)

// schemaLookup returns a map keyed by tool name of the parsed InputSchema for
// each tool the MCP server advertised. The MCP Go SDK reports InputSchema as
// an `any` on the client side (typically map[string]any), so we round-trip
// through JSON to rehydrate into *jsonschema.Schema. Tools whose schema cannot
// be parsed are simply omitted; callers treat a missing entry as "no schema"
// and skip coercion for that tool, preserving current behavior.
func schemaLookup(tools []*mcp.Tool) map[string]*jsonschema.Schema {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]*jsonschema.Schema, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.InputSchema == nil {
			continue
		}
		schema, err := parseInputSchema(tool.InputSchema)
		if err != nil || schema == nil {
			continue
		}
		out[tool.Name] = schema
	}
	return out
}

func parseInputSchema(raw any) (*jsonschema.Schema, error) {
	switch v := raw.(type) {
	case *jsonschema.Schema:
		return v, nil
	case jsonschema.Schema:
		out := v
		return &out, nil
	}
	bytes, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(bytes, &schema); err != nil {
		return nil, err
	}
	return &schema, nil
}

// coerceArgumentsToSchema rewrites tool-call arguments in place so each field
// matches the type declared in the tool's JSON Schema when a conservative
// coercion is possible. Fields with no schema entry, fields whose current
// value already satisfies the declared type, and fields whose coerced value
// would violate `enum` or numeric bounds are left untouched.
func coerceArgumentsToSchema(toolName string, arguments map[string]any, schemas map[string]*jsonschema.Schema) {
	if len(arguments) == 0 || len(schemas) == 0 {
		return
	}
	schema := schemas[toolName]
	if schema == nil {
		return
	}
	coerceObject(arguments, schema, toolName)
}

// coerceObject iterates an argument map and rewrites each property whose
// declared schema suggests a different primitive type than the supplied value.
// It also descends into nested objects and array items so deeply-nested
// fields (for example, filters.allowed_domains[]) benefit from the same
// treatment.
func coerceObject(obj map[string]any, schema *jsonschema.Schema, path string) {
	if obj == nil || schema == nil {
		return
	}
	for key, value := range obj {
		propSchema := propertySchema(schema, key)
		if propSchema == nil {
			continue
		}
		newValue, changed := coerceValue(value, propSchema)
		if changed {
			obj[key] = newValue
			debugLogf("toolruntime:coerce tool=%s field=%s.%s %T->%T", path, path, key, value, newValue)
		}
	}
}

// propertySchema returns the sub-schema for a named property, honoring the
// main Properties map, patternProperties (simple prefix match only), and the
// allOf/anyOf/oneOf composition keywords. Returns nil when no declaration
// applies, which callers treat as "pass through unchanged".
func propertySchema(parent *jsonschema.Schema, key string) *jsonschema.Schema {
	if parent == nil {
		return nil
	}
	if sub, ok := parent.Properties[key]; ok && sub != nil {
		return sub
	}
	for _, branch := range parent.AllOf {
		if sub := propertySchema(branch, key); sub != nil {
			return sub
		}
	}
	for _, branch := range parent.AnyOf {
		if sub := propertySchema(branch, key); sub != nil {
			return sub
		}
	}
	for _, branch := range parent.OneOf {
		if sub := propertySchema(branch, key); sub != nil {
			return sub
		}
	}
	return nil
}

// declaredTypes returns the JSON Schema type keywords that apply to the given
// schema. Schemas can declare a single type via Type or multiple via Types;
// when neither is set, callers treat the schema as accepting any type. This
// helper never returns duplicates.
func declaredTypes(schema *jsonschema.Schema) []string {
	if schema == nil {
		return nil
	}
	if schema.Type != "" {
		return []string{schema.Type}
	}
	if len(schema.Types) == 0 {
		return nil
	}
	out := make([]string, 0, len(schema.Types))
	seen := make(map[string]struct{}, len(schema.Types))
	for _, t := range schema.Types {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// coerceValue returns the value transformed to match the schema's declared
// type along with a boolean indicating whether a rewrite actually occurred.
// When the value already satisfies the schema, when no coercion rule
// applies, or when the candidate replacement would violate enum or range
// constraints, the original value is returned unchanged.
func coerceValue(value any, schema *jsonschema.Schema) (any, bool) {
	if schema == nil {
		return value, false
	}
	types := declaredTypes(schema)
	if len(types) == 0 {
		return descendIntoContainers(value, schema)
	}
	if valueMatchesAnyType(value, types) {
		return descendIntoContainers(value, schema)
	}
	for _, t := range types {
		switch t {
		case "integer":
			if coerced, ok := coerceToInteger(value, schema); ok {
				return coerced, true
			}
		case "number":
			if coerced, ok := coerceToNumber(value, schema); ok {
				return coerced, true
			}
		case "boolean":
			if coerced, ok := coerceToBoolean(value); ok {
				return coerced, true
			}
		case "string":
			if coerced, ok := coerceToString(value); ok {
				return coerced, true
			}
		case "array":
			if coerced, ok := coerceToArray(value, schema); ok {
				return coerced, true
			}
		case "object":
			if coerced, ok := coerceToObject(value, schema); ok {
				return coerced, true
			}
		case "null":
			if coerced, ok := coerceToNull(value); ok {
				return coerced, true
			}
		}
	}
	return descendIntoContainers(value, schema)
}

// descendIntoContainers recurses into already-correctly-typed objects and
// arrays so nested fields still receive coercion. For primitives it is a
// no-op. This is the branch taken when a value already matches the schema's
// declared type or when no type is declared at all; the containers still may
// hold stringified leaves that need fixing.
func descendIntoContainers(value any, schema *jsonschema.Schema) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		coerceObject(v, schema, "object")
		return v, false
	case []any:
		if schema.Items != nil {
			for i, item := range v {
				if item, changed := coerceValue(item, schema.Items); changed {
					v[i] = item
				}
			}
		}
		return v, false
	default:
		return value, false
	}
}

// valueMatchesAnyType reports whether the supplied value is already one of
// the declared JSON Schema types without needing coercion.
func valueMatchesAnyType(value any, types []string) bool {
	for _, t := range types {
		if valueMatchesType(value, t) {
			return true
		}
	}
	return false
}

func valueMatchesType(value any, t string) bool {
	switch t {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		return isIntegerValue(value)
	case "number":
		return isNumericValue(value)
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "null":
		return value == nil
	}
	return false
}

func isNumericValue(value any) bool {
	switch value.(type) {
	case float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return true
	}
	return false
}

// isIntegerValue reports whether the value is already a Go integer type.
// Whole-number floats are deliberately excluded so they flow through
// coerceToInteger and become int64 for strict Go validators that check
// runtime type rather than numeric value alone.
func isIntegerValue(value any) bool {
	switch v := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case json.Number:
		if _, err := v.Int64(); err == nil {
			return true
		}
		return false
	}
	return false
}

// coerceToInteger attempts to turn the value into an int64 that satisfies the
// schema's enum and range constraints. Strings are parsed as base-10
// integers; json.Number is consulted directly; floats are accepted when they
// represent a whole number within coerceMaxIntFromFloat.
func coerceToInteger(value any, schema *jsonschema.Schema) (int64, bool) {
	switch v := value.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			if integerSatisfiesSchema(n, schema) {
				return n, true
			}
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			if n, ok := floatToBoundedInt64(f); ok && integerSatisfiesSchema(n, schema) {
				return n, true
			}
		}
	case float32:
		if n, ok := floatToBoundedInt64(float64(v)); ok && integerSatisfiesSchema(n, schema) {
			return n, true
		}
	case float64:
		if n, ok := floatToBoundedInt64(v); ok && integerSatisfiesSchema(n, schema) {
			return n, true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			if integerSatisfiesSchema(n, schema) {
				return n, true
			}
		}
	}
	return 0, false
}

// floatToBoundedInt64 converts a float64 to int64 only when the value is a
// finite whole number whose magnitude is within coerceMaxIntFromFloat. NaN,
// +/-Inf, fractional values, and anything that would overflow int64 via
// Go's conversion rules are rejected so callers fall back to leaving the
// original value untouched. The bound is the same one applied to the
// string-parsing branch; keeping it here means every float-origin path
// (string-parsed, float32, float64) honors the documented guarantee.
func floatToBoundedInt64(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if math.Trunc(f) != f {
		return 0, false
	}
	if math.Abs(f) > coerceMaxIntFromFloat {
		return 0, false
	}
	return int64(f), true
}

// coerceToNumber attempts to turn the value into a float64 that satisfies the
// schema's enum and range constraints. Strings are parsed with ParseFloat;
// json.Number is converted via Float64.
func coerceToNumber(value any, schema *jsonschema.Schema) (float64, bool) {
	switch v := value.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			if numberSatisfiesSchema(f, schema) {
				return f, true
			}
		}
	case json.Number:
		if f, err := v.Float64(); err == nil {
			if numberSatisfiesSchema(f, schema) {
				return f, true
			}
		}
	}
	return 0, false
}

// coerceToBoolean accepts the common human-readable renderings for truthy
// values so upstream models that emit booleans as quoted strings still fire
// the intended behavior. Unknown strings leave the original value in place.
func coerceToBoolean(value any) (bool, bool) {
	switch v := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "on", "y", "t":
			return true, true
		case "false", "0", "no", "off", "n", "f":
			return false, true
		}
	case float64:
		if v == 1 {
			return true, true
		}
		if v == 0 {
			return false, true
		}
	case int:
		if v == 1 {
			return true, true
		}
		if v == 0 {
			return false, true
		}
	}
	return false, false
}

func coerceToString(value any) (string, bool) {
	switch v := value.(type) {
	case float64:
		if v == math.Trunc(v) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return v.String(), true
	}
	return "", false
}

// coerceToArray converts a string-encoded array (either a JSON literal or a
// comma-separated list) into a []any and then recurses into each element so
// typed array items also get coerced. A value that is already an array is
// descended into without being replaced.
func coerceToArray(value any, schema *jsonschema.Schema) ([]any, bool) {
	switch v := value.(type) {
	case []any:
		if schema.Items != nil {
			for i, item := range v {
				if item, changed := coerceValue(item, schema.Items); changed {
					v[i] = item
				}
			}
		}
		return v, false
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, false
		}
		var parsed []any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			if schema.Items != nil {
				for i, item := range parsed {
					if item, changed := coerceValue(item, schema.Items); changed {
						parsed[i] = item
					}
				}
			}
			return parsed, true
		}
		if looksLikeCSV(s) {
			parts := splitCSV(s)
			arr := make([]any, 0, len(parts))
			for _, part := range parts {
				arr = append(arr, part)
			}
			if schema.Items != nil {
				for i, item := range arr {
					if item, changed := coerceValue(item, schema.Items); changed {
						arr[i] = item
					}
				}
			}
			return arr, true
		}
	}
	return nil, false
}

func coerceToObject(value any, schema *jsonschema.Schema) (map[string]any, bool) {
	switch v := value.(type) {
	case map[string]any:
		coerceObject(v, schema, "object")
		return v, false
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, false
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			coerceObject(parsed, schema, "object")
			return parsed, true
		}
	}
	return nil, false
}

func coerceToNull(value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	if s, ok := value.(string); ok {
		if strings.EqualFold(strings.TrimSpace(s), "null") {
			return nil, true
		}
	}
	return nil, false
}

func integerSatisfiesSchema(n int64, schema *jsonschema.Schema) bool {
	if schema.Minimum != nil && float64(n) < *schema.Minimum {
		return false
	}
	if schema.Maximum != nil && float64(n) > *schema.Maximum {
		return false
	}
	if schema.ExclusiveMinimum != nil && float64(n) <= *schema.ExclusiveMinimum {
		return false
	}
	if schema.ExclusiveMaximum != nil && float64(n) >= *schema.ExclusiveMaximum {
		return false
	}
	if schema.MultipleOf != nil && *schema.MultipleOf != 0 {
		if math.Mod(float64(n), *schema.MultipleOf) != 0 {
			return false
		}
	}
	return enumContainsNumeric(schema.Enum, float64(n))
}

func numberSatisfiesSchema(f float64, schema *jsonschema.Schema) bool {
	if schema.Minimum != nil && f < *schema.Minimum {
		return false
	}
	if schema.Maximum != nil && f > *schema.Maximum {
		return false
	}
	if schema.ExclusiveMinimum != nil && f <= *schema.ExclusiveMinimum {
		return false
	}
	if schema.ExclusiveMaximum != nil && f >= *schema.ExclusiveMaximum {
		return false
	}
	if schema.MultipleOf != nil && *schema.MultipleOf != 0 {
		if math.Mod(f, *schema.MultipleOf) != 0 {
			return false
		}
	}
	return enumContainsNumeric(schema.Enum, f)
}

// enumContainsNumeric returns true when the schema either declares no enum or
// when the candidate is numerically equal to one of the listed values. Enum
// entries can be any JSON value; we compare by number when both sides are
// numeric, otherwise we fall back to string-eq for robustness.
func enumContainsNumeric(enum []any, candidate float64) bool {
	if len(enum) == 0 {
		return true
	}
	for _, entry := range enum {
		switch e := entry.(type) {
		case float64:
			if e == candidate {
				return true
			}
		case int:
			if float64(e) == candidate {
				return true
			}
		case int64:
			if float64(e) == candidate {
				return true
			}
		case json.Number:
			if f, err := e.Float64(); err == nil && f == candidate {
				return true
			}
		}
	}
	return false
}

// splitCSV is a minimal CSV splitter covering the "allowed_domains" style
// comma-separated list pattern. It intentionally does not handle quoted
// fields; complex values should come in as a JSON array string, which the
// JSON branch in coerceToArray handles first.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// looksLikeCSV is a conservative check that keeps us from treating
// single-token strings like "python.org" as arrays. An argument only counts
// as CSV when it contains at least one comma and produces two or more
// non-empty fields after trimming.
func looksLikeCSV(s string) bool {
	if !strings.Contains(s, ",") {
		return false
	}
	parts := splitCSV(s)
	return len(parts) >= 2
}

// humanizeToolArgError rewrites the raw server-side validator error into a
// short, actionable preamble that upstream models can actually parse and
// correct on their next turn. It falls back to the original error when the
// shape we expect is not present.
func humanizeToolArgError(toolName string, err error, arguments map[string]any) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	summary := extractValidatorSummary(raw)
	if summary == "" {
		return fmt.Sprintf("Tool %q rejected the arguments: %s", toolName, raw)
	}
	argsPreview := debugPreview(arguments, 400)
	if argsPreview == "" {
		if b, marshalErr := json.Marshal(arguments); marshalErr == nil {
			argsPreview = string(b)
		}
	}
	return fmt.Sprintf("Tool %q rejected the arguments. %s Original args: %s", toolName, summary, argsPreview)
}

// extractValidatorSummary plucks the salient `/properties/<field>` and
// type-mismatch text out of a raw jsonschema-go / MCP validator error so the
// model can see which field needs fixing without wading through the full
// schema path. Matches the common "validating /properties/<field>: type: X
// has type \"Y\", want \"Z\"" form produced by the upstream library.
func extractValidatorSummary(raw string) string {
	fieldIdx := strings.Index(raw, "/properties/")
	if fieldIdx == -1 {
		return ""
	}
	tail := raw[fieldIdx+len("/properties/"):]
	colon := strings.Index(tail, ":")
	if colon == -1 {
		return ""
	}
	field := strings.TrimSpace(tail[:colon])
	rest := tail[colon+1:]
	typeIdx := strings.Index(rest, "type:")
	if typeIdx == -1 {
		return fmt.Sprintf("Invalid field %q.", field)
	}
	expected, actualQuoted := parseTypeMismatch(rest[typeIdx+len("type:"):])
	if expected == "" || actualQuoted == "" {
		return fmt.Sprintf("Invalid field %q.", field)
	}
	return fmt.Sprintf("Field %q must be %s (received %s). Retry with the corrected type.", field, expected, actualQuoted)
}

// parseTypeMismatch pulls "<actual>" and <expected> out of a jsonschema-go
// "type: VALUE has type \"ACTUAL\", want \"EXPECTED\"" fragment.
func parseTypeMismatch(s string) (expected, actualQuoted string) {
	wantIdx := strings.Index(s, "want ")
	if wantIdx == -1 {
		return "", ""
	}
	want := strings.TrimSpace(s[wantIdx+len("want "):])
	want = strings.Trim(want, "\"' .,")
	has := strings.Index(s[:wantIdx], "has type ")
	if has == -1 {
		return want, ""
	}
	actual := strings.TrimSpace(s[has+len("has type ") : wantIdx])
	actual = strings.TrimRight(actual, " ,")
	return want, actual
}
