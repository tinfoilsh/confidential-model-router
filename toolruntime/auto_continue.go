package toolruntime

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
)

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
