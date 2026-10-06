package toolruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Auto-continue client tools.
//
// The OpenAI tool-call protocol was designed for *function* tools — ones
// where the model asks the host to go do something real (query a DB, hit
// an API, run code) and report back. The protocol ends the model's turn
// after the tool call because the host needs to produce a result the
// model didn't have. The model cannot finish its answer until that
// result exists.
//
// Some classes of tools don't fit that shape. Generative-UI render tools,
// for example, have no real side effect: the result is always "executed".
// The round trip exists solely to satisfy a protocol that was designed for
// a different problem. With a vanilla relay, the model emits a tool call,
// the turn ends, the client renders a widget — and the user never sees the
// prose the model was about to write. The answer "craps out" mid-thought.
//
// A caller flags such tools by setting `x-tinfoil-tool-auto-continue: true`
// on the tool definition in the request body. The router strips that field
// before forwarding the request upstream (so the model never sees router-
// specific fields) and remembers the tool names. When the model later
// emits a call to one of those tools, the tool-loop driver synthesises
// the result, appends it to history, and re-invokes the model so it can
// produce the surrounding prose. The client sees a single coherent
// response that contains both the tool call and the follow-up content.
//
// Because the client never reports back, the router is the only party
// that can tell the model its arguments were unusable. The tool's
// `parameters` schema is therefore kept and the arguments validated
// before the synthetic result is chosen: a schema violation yields an
// error result naming the field so the model can call the tool again.
//
// Three classes of tools end up flowing through the router:
//
//   1. Router-owned (web_search, fetch): registered via MCP profiles. The
//      router knows how to execute them and what to return.
//   2. Auto-continue client tools: declared per-request with the flag in
//      this file. The router synthesises a constant result and continues
//      the loop. The widget renderer lives on the client.
//   3. Real client function tools: any other tool the caller declares.
//      The router cannot synthesise a result, so the turn finalizes and
//      the caller is responsible for executing the tool and re-posting.

const (
	// autoContinueToolFlag is the per-tool boolean field a caller sets on
	// an OpenAI tool definition to opt a tool into router-side auto-
	// continuation. The router strips this field before forwarding the
	// request upstream because it is router-internal metadata.
	autoContinueToolFlag = "x-tinfoil-tool-auto-continue"

	// autoContinueToolResult is the synthetic tool result the router
	// injects into history after every auto-continue tool call so the
	// upstream model has the protocol-mandated `role:"tool"` /
	// `function_call_output` entry it needs to continue generating.
	// The string is intentionally inert: just enough for the model to
	// know the call succeeded, with no content that could influence the
	// follow-up answer.
	autoContinueToolResult = `{"status":"executed"}`

	// maxAutoContinueSchemaRetries bounds how many times per request a
	// given auto-continue tool is told its arguments failed validation.
	// Past this the router acknowledges the call as executed so a model
	// that cannot satisfy the schema moves on instead of burning the whole
	// tool budget regenerating the same widget.
	maxAutoContinueSchemaRetries = 1
)

// autoContinueConfig is what a request's tools array tells the router
// about auto-continue tools: which names to acknowledge, and the resolved
// parameter schema for each so arguments can be checked before the ack.
type autoContinueConfig struct {
	tools   map[string]struct{}
	schemas map[string]*jsonschema.Resolved
}

// autoContinueSchemas resolves the JSON Schema `parameters` of every tool
// in names so the router can validate the model's arguments before
// acknowledging the call. Chat tools nest the definition under `function`;
// the Responses shape is flat. Tools whose schema fails to parse or
// resolve are omitted and simply never validated.
func autoContinueSchemas(rawTools any, names map[string]struct{}) map[string]*jsonschema.Resolved {
	tools, _ := rawTools.([]any)
	if len(tools) == 0 || len(names) == 0 {
		return nil
	}
	out := make(map[string]*jsonschema.Resolved, len(names))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		def := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			def = fn
		}
		name := stringValue(def["name"])
		if _, flagged := names[name]; !flagged {
			continue
		}
		params := def["parameters"]
		if params == nil {
			continue
		}
		schema, err := parseInputSchema(params)
		if err != nil || schema == nil {
			continue
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			debugLogf("toolruntime:auto_continue schema_unresolvable tool=%s err=%v", name, err)
			continue
		}
		out[name] = resolved
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// autoContinueSchemaFailures counts, per tool name, how many times the
// router has already returned a validation error for one request.
type autoContinueSchemaFailures map[string]int

// autoContinueToolOutput returns the synthetic tool result for one
// auto-continue call. Arguments that satisfy the tool's schema (or belong
// to a tool with no resolvable schema) are acknowledged as executed. When
// they do not, the model receives a structured error naming the offending
// field so it can call the tool again with corrected arguments, which is
// the only way for it to learn the client could not render the widget.
func autoContinueToolOutput(call toolCall, schemas map[string]*jsonschema.Resolved, failures autoContinueSchemaFailures) string {
	resolved := schemas[call.name]
	if resolved == nil {
		return autoContinueToolResult
	}
	problem := validateAutoContinueArguments(call.arguments, resolved)
	if problem == "" {
		return autoContinueToolResult
	}
	if failures[call.name] >= maxAutoContinueSchemaRetries {
		debugLogf("toolruntime:auto_continue schema_invalid tool=%s retries_exhausted problem=%s", call.name, problem)
		return autoContinueToolResult
	}
	failures[call.name]++
	debugLogf("toolruntime:auto_continue schema_invalid tool=%s problem=%s", call.name, problem)
	payload, err := json.Marshal(map[string]any{
		"status": "error",
		"error": fmt.Sprintf(
			"The arguments for %s did not match the tool's parameter schema and the component was not displayed: %s. Call %s again with corrected arguments that match the schema exactly.",
			call.name, problem, call.name,
		),
	})
	if err != nil {
		return autoContinueToolResult
	}
	return string(payload)
}

// validateAutoContinueArguments checks arguments against the resolved
// schema and returns a short description of the first problem, or "" when
// the arguments are valid. The arguments are canonicalized first so the
// router validates the same shape it forwards to the client (see
// canonicalizeAutoContinueArguments), and round-tripped through plain
// json.Unmarshal so numbers are float64, which the validator understands,
// rather than json.Number, which it treats as a string.
func validateAutoContinueArguments(arguments map[string]any, resolved *jsonschema.Resolved) string {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return ""
	}
	var instance any
	if err := json.Unmarshal([]byte(canonicalizeAutoContinueArguments(string(encoded))), &instance); err != nil {
		return ""
	}
	err = resolved.Validate(instance)
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), "validating root: ")
}

// HasAutoContinueTools reports whether the request declares any client tool
// the router should auto-acknowledge and continue. Main uses this to route
// auto-continue-only requests through toolruntime even when no MCP-backed
// router tools (for example web_search) are active.
func HasAutoContinueTools(path string, body map[string]any) bool {
	if body == nil {
		return false
	}
	switch path {
	case "/v1/chat/completions":
		return hasAutoContinueChatTool(body["tools"])
	case "/v1/responses":
		return hasAutoContinueResponsesTool(body["tools"])
	default:
		return false
	}
}

func hasAutoContinueChatTool(rawTools any) bool {
	tools, _ := rawTools.([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if flagged, _ := fn[autoContinueToolFlag].(bool); flagged {
			return true
		}
	}
	return false
}

func hasAutoContinueResponsesTool(rawTools any) bool {
	tools, _ := rawTools.([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		if flagged, _ := tool[autoContinueToolFlag].(bool); flagged {
			return true
		}
	}
	return false
}

// extractAndStripAutoContinueChatTools walks a chat-completions tools array,
// records every tool whose function definition carries the auto-continue
// flag, and removes the flag in place so the upstream model never sees a
// router-internal field. The returned set is keyed by tool name; an entry
// is present iff the caller flagged that tool as auto-continue on this
// request.
func extractAndStripAutoContinueChatTools(rawTools any) map[string]struct{} {
	tools, _ := rawTools.([]any)
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]struct{})
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		flagged, hasFlag := fn[autoContinueToolFlag].(bool)
		if hasFlag {
			delete(fn, autoContinueToolFlag)
		}
		if !flagged {
			continue
		}
		name := stringValue(fn["name"])
		if name == "" {
			continue
		}
		out[name] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractAndStripAutoContinueResponsesTools mirrors
// extractAndStripAutoContinueChatTools for the /v1/responses tools array
// shape, where each tool is a flat map (no nested `function` block).
func extractAndStripAutoContinueResponsesTools(rawTools any) map[string]struct{} {
	tools, _ := rawTools.([]any)
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]struct{})
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		flagged, hasFlag := tool[autoContinueToolFlag].(bool)
		if hasFlag {
			delete(tool, autoContinueToolFlag)
		}
		if !flagged {
			continue
		}
		name := stringValue(tool["name"])
		if name == "" {
			continue
		}
		out[name] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// canonicalizeAutoContinueArguments normalises a tool-call `arguments`
// payload before the router carries it back to the client. Some upstream
// providers (notably DeepSeek-V4-Pro on non-streaming responses) emit
// nested arrays and objects as JSON-encoded strings, e.g.
// `{"stats":"[{\"label\":\"a\"}]"}` instead of `{"stats":[{"label":"a"}]}`.
// Client widget schemas reject the stringified shape, which surfaces to
// the user as "couldn't display this widget".
//
// vLLM's `--enable-auto-tool-choice` path does not run guided decoding
// over function arguments, so OpenAI-style `strict: true` cannot fix the
// quirk at the source today. Doing the unwrap once on the router lets
// every client (web, iOS, future surfaces) stay shape-agnostic instead
// of duplicating the same workaround.
//
// The transform is intentionally narrow:
//   - only applied to arguments produced by tools the caller flagged with
//     `x-tinfoil-tool-auto-continue: true` (`filterChatRawToolCalls` and
//     `filterResponsesOutputItemsForToolCalls` are the only call sites);
//   - only unwraps strings that look structurally like JSON arrays or
//     objects (leading `{` / `[` after trimming);
//   - leaves the input untouched on any parse failure;
//   - idempotent: already-canonical arguments round-trip unchanged.
//
// Removal criteria: this function and `deepUnwrapJSONStrings` can be
// deleted once every upstream provider the router serves emits tool-call
// `arguments` with native JSON types for nested arrays/objects. Concretely
// that means either:
//  1. vLLM's `--enable-auto-tool-choice` runs guided decoding over function
//     arguments (so `strict: true` on the tool schema constrains the shape
//     at the source), and every served model is migrated onto that path; or
//  2. the affected models (DeepSeek-V4-Pro and any future variants that
//     exhibit the same stringification quirk) are retired or replaced.
//
// When removing, also drop the call sites in `filterChatRawToolCalls` /
// `filterResponsesOutputItemsForToolCalls` and the canonicaliser tests in
// `auto_continue_test.go`.
func canonicalizeAutoContinueArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return raw
	}
	decoded, err := decodeJSONValue(trimmed)
	if err != nil {
		return raw
	}
	unwrapped, changed := deepUnwrapJSONStrings(decoded)
	if !changed {
		// Re-encoding would reorder keys and drop the model's original
		// formatting; a payload that needed no unwrapping is forwarded
		// byte-for-byte.
		return raw
	}
	encoded, err := json.Marshal(unwrapped)
	if err != nil {
		return raw
	}
	return string(encoded)
}

// deepUnwrapJSONStrings walks a value decoded from a tool-call arguments
// payload and replaces any string whose contents are themselves valid JSON
// for an array or object with the decoded structure. Plain strings,
// numbers, bools, and nulls pass through unchanged. Map keys are not
// rewritten. The boolean reports whether any string was unwrapped.
//
// Removal criteria: see `canonicalizeAutoContinueArguments`. This helper
// has no other call sites and goes away with it.
func deepUnwrapJSONStrings(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return v, false
		}
		if trimmed[0] != '{' && trimmed[0] != '[' {
			return v, false
		}
		nested, err := decodeJSONValue(trimmed)
		if err != nil {
			return v, false
		}
		unwrapped, _ := deepUnwrapJSONStrings(nested)
		return unwrapped, true
	case []any:
		out := make([]any, len(v))
		changed := false
		for i, item := range v {
			var itemChanged bool
			out[i], itemChanged = deepUnwrapJSONStrings(item)
			changed = changed || itemChanged
		}
		return out, changed
	case map[string]any:
		out := make(map[string]any, len(v))
		changed := false
		for k, item := range v {
			var itemChanged bool
			out[k], itemChanged = deepUnwrapJSONStrings(item)
			changed = changed || itemChanged
		}
		return out, changed
	default:
		return value, false
	}
}

func decodeJSONValue(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return decoded, nil
}

// splitClientToolCalls separates plain client-owned tool calls from the
// auto-continue subset. Auto-continue calls are handled by the router
// (synthetic result + loop continuation); the rest finalize the turn so
// the caller can execute them and re-post.
func splitClientToolCalls(
	autoContinue map[string]struct{},
	clientCalls []toolCall,
) (continued []toolCall, external []toolCall) {
	if len(clientCalls) == 0 {
		return nil, nil
	}
	for _, call := range clientCalls {
		if _, ok := autoContinue[call.name]; ok {
			continued = append(continued, call)
			continue
		}
		external = append(external, call)
	}
	return continued, external
}
