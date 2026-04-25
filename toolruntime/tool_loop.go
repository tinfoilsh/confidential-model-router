package toolruntime

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

// toolLoopAdapter abstracts the two OpenAI surfaces (Chat Completions and
// Responses) behind a single interface so the shared driver can walk the
// iteration, usage-accumulation, and finalize logic without caring which
// shape the caller asked for. Each adapter owns:
//   - the upstream path and its forced-final request shape;
//   - extracting the router-owned tool calls from the upstream response;
//   - folding tool outputs back into the next-iteration request body;
//   - the per-surface `includeActionSources` decision (Chat never, Responses
//     by caller opt-in) so the shared finalize path stays uniform.
type toolLoopAdapter interface {
	upstreamPath() string

	// buildInitialRequest produces the first-turn request body with the
	// router's prompt prefix applied and streaming disabled. The returned
	// map is mutated across iterations.
	buildInitialRequest() map[string]any

	// includeActionSources reports whether finalize should synthesize the
	// spec-only `action.sources` array on web_search_call output items.
	includeActionSources() bool

	tracePhase(iteration int) string
	traceID() string

	// preIteration runs surface-specific housekeeping before each upstream
	// POST (e.g. Chat sanitizes assistant tool_calls in accumulated
	// history so replay stays protocol-valid).
	preIteration(reqBody map[string]any, iteration int)

	// onUpstreamResponse extracts router-owned tool calls plus any
	// surface-specific state (Chat: assistant message; Responses: raw
	// output items) that applyToolOutputs will fold back into the next
	// request body. hasClientToolCalls reports whether the same turn
	// also carried client-owned tool calls; see the mixed-turn contract
	// in runToolLoop. elapsed is the upstream call's wall time.
	onUpstreamResponse(response *upstreamJSONResponse, iteration int, elapsed time.Duration) (routerToolCalls []toolCall, hasClientToolCalls bool, state any)

	applyToolOutputs(reqBody map[string]any, state any, outputs []toolOutput) map[string]any

	// stripRouterToolCallsFromResponse removes router-owned tool calls
	// from a mixed-turn response so the caller sees only the client-owned
	// calls that still need a response. Router work is surfaced via
	// attachCitations instead.
	stripRouterToolCallsFromResponse(response *upstreamJSONResponse)

	// forcedFinalRequest builds the forced-final-answer request body used
	// when the iteration budget is exhausted without the model settling.
	forcedFinalRequest(reqBody map[string]any) map[string]any

	// applyUsage writes the surface-specific usage shape onto
	// response.body. Chat uses prompt_tokens/completion_tokens/total_tokens;
	// Responses uses input_tokens/output_tokens/total_tokens.
	applyUsage(response *upstreamJSONResponse, usage *tokencount.Usage)

	// attachCitations resolves inline markdown links and surfaces
	// router-owned tool progress in the surface-specific shape.
	attachCitations(body map[string]any, citations *citationState, toolCalls *toolCallLog, eventFlags tinfoilEventFlags)

	options() webSearchOptions
	schemas() map[string]*jsonschema.Schema
}

// toolOutput pairs an upstream tool call's id/name with the text the router
// produced for it so adapters can wrap the output in the surface-specific
// message shape (Chat `role:"tool"` vs Responses `function_call_output`).
type toolOutput struct {
	callID string
	name   string
	output string
}

// runToolLoop drives the iteration loop shared by the chat and responses
// surfaces. The adapter owns everything that differs between them; the
// driver owns the invariants: per-iteration POST + usage folding, the
// "no router tool calls this turn -> finalize and return" contract, the
// executeRouterToolCall dispatch, and the iteration-budget / forced-final
// fallback with its usage-accumulation comment.
//
// Mixed-turn contract: when an assistant turn emits both router-owned
// and client-owned tool calls, the router resolves its own calls and
// then finalizes instead of replaying. Replaying would send back an
// assistant message whose client tool_calls have no matching
// role:"tool" / function_call_output entries; upstream then either
// rejects the request or drops the client call. On finalize the
// caller sees the client tool_calls verbatim and the router's work
// surfaced via attachCitations.
func runToolLoop(
	ctx context.Context,
	em *manager.EnclaveManager,
	registry *sessionRegistry,
	modelName string,
	requestHeaders http.Header,
	adapter toolLoopAdapter,
	eventFlags tinfoilEventFlags,
	dl *devLog,
) (*upstreamJSONResponse, error) {
	reqBody := adapter.buildInitialRequest()
	usageTotals := usageAccumulator{}
	citations := citationState{nextIndex: 1}
	toolCalls := toolCallLog{}
	opts := adapter.options()
	toolSchemas := adapter.schemas()
	path := adapter.upstreamPath()

	for i := 0; i < maxToolIterations; i++ {
		adapter.preIteration(reqBody, i)

		start := time.Now()
		if traceID := adapter.traceID(); traceID != "" {
			if msgs, ok := reqBody["messages"].([]any); ok {
				debugLogf("toolruntime:%s %s upstream.request messages_n=%d history=%s",
					traceID, adapter.tracePhase(i), len(msgs), debugMessagesSummary(msgs, 200))
			}
		}

		response, err := postJSON(ctx, em, modelName, path, reqBody, requestHeaders)
		if err != nil {
			if traceID := adapter.traceID(); traceID != "" {
				debugLogf("toolruntime:%s %s upstream.error elapsed=%s err=%v", traceID, adapter.tracePhase(i), time.Since(start), err)
			}
			return nil, err
		}
		usageTotals.Add(response)

		routerToolCalls, hasClientToolCalls, state := adapter.onUpstreamResponse(response, i, time.Since(start))

		// Dev log: turn header + tokens + thinking + content
		dl.WriteTurnHeader(i + 1)
		if usage, ok := response.body["usage"].(map[string]any); ok {
			dl.WriteTokens(usage)
		}
		thinking, content := extractThinkingAndContent(response.body)
		dl.WriteThinking(thinking)
		dl.WriteContent(content)

		if len(routerToolCalls) == 0 {
			if traceID := adapter.traceID(); traceID != "" {
				debugLogf("toolruntime:%s %s done iterations=%d (no router tool calls this turn)", traceID, adapter.tracePhase(i), i+1)
			}
			dl.WriteFinish("stop (no router tool calls)")
			adapter.applyUsage(response, usageTotals.Usage())
			adapter.attachCitations(response.body, &citations, &toolCalls, eventFlags)
			return response, nil
		}

		dl.WriteToolCalls(routerToolCalls)
		outputs := make([]toolOutput, 0, len(routerToolCalls))
		for _, call := range routerToolCalls {
			tstart := time.Now()
			output := executeRouterToolCall(ctx, registry, call, opts, toolSchemas, &citations, &toolCalls, adapter.tracePhase(i), adapter.traceID())
			dl.WriteToolExec(call.name, call.arguments, output, time.Since(tstart), "")
			outputs = append(outputs, toolOutput{
				callID: call.id,
				name:   call.name,
				output: output,
			})
		}

		// Mixed turn: finalize instead of replaying so the client's
		// tool calls are not orphaned in the next request's history.
		if hasClientToolCalls {
			if traceID := adapter.traceID(); traceID != "" {
				debugLogf("toolruntime:%s %s mixed_turn iterations=%d (router+client calls in one turn; finalizing without replay)", traceID, adapter.tracePhase(i), i+1)
			}
			adapter.stripRouterToolCallsFromResponse(response)
			adapter.applyUsage(response, usageTotals.Usage())
			adapter.attachCitations(response.body, &citations, &toolCalls, eventFlags)
			return response, nil
		}

		reqBody = adapter.applyToolOutputs(reqBody, state, outputs)
	}

	if traceID := adapter.traceID(); traceID != "" {
		debugLogf("toolruntime:%s %s exhausted iterations=%d forcing final answer", traceID, adapter.tracePhase(maxToolIterations), maxToolIterations)
	}
	finalResponse, err := postJSON(ctx, em, modelName, path, adapter.forcedFinalRequest(reqBody), requestHeaders)
	if err != nil {
		return nil, err
	}
	// The forced-final turn consumes tokens too; feed them into the
	// accumulator before finalize overwrites response.body["usage"] with
	// the aggregated totals. Without this, callers billed for the tool
	// budget would undercount by exactly the final-answer turn.
	usageTotals.Add(finalResponse)
	adapter.applyUsage(finalResponse, usageTotals.Usage())
	adapter.attachCitations(finalResponse.body, &citations, &toolCalls, eventFlags)
	return finalResponse, nil
}

// chatLoopAdapter drives /v1/chat/completions: it owns the OpenAI-compatible
// messages history shape, the trace id threaded through debug logs, and the
// per-iteration assistant-history sanitization quirk.
type chatLoopAdapter struct {
	body           map[string]any
	prompt         *mcp.GetPromptResult
	tools          []*mcp.Tool
	ownedTools     map[string]struct{}
	modelName      string
	requestHeaders http.Header
	tid            string
	opts           webSearchOptions
	toolSchemas    map[string]*jsonschema.Schema
}

func newChatLoopAdapter(body map[string]any, prompt *mcp.GetPromptResult, tools []*mcp.Tool, ownedTools map[string]struct{}, modelName string, requestHeaders http.Header) *chatLoopAdapter {
	return &chatLoopAdapter{
		body:           body,
		prompt:         prompt,
		tools:          tools,
		ownedTools:     ownedTools,
		modelName:      modelName,
		requestHeaders: requestHeaders,
		tid:            debugTraceID(),
		opts:           parseChatWebSearchOptions(body),
		toolSchemas:    schemaLookup(tools),
	}
}

func (a *chatLoopAdapter) upstreamPath() string { return "/v1/chat/completions" }
func (a *chatLoopAdapter) traceID() string      { return a.tid }
func (a *chatLoopAdapter) tracePhase(iteration int) string {
	return fmt.Sprintf("chat.iter=%d", iteration)
}
func (a *chatLoopAdapter) includeActionSources() bool { return false }
func (a *chatLoopAdapter) options() webSearchOptions  { return a.opts }
func (a *chatLoopAdapter) schemas() map[string]*jsonschema.Schema {
	return a.toolSchemas
}

func (a *chatLoopAdapter) applyUsage(response *upstreamJSONResponse, usage *tokencount.Usage) {
	if response == nil || response.body == nil || usage == nil {
		return
	}
	response.body["usage"] = map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
	}
}

func (a *chatLoopAdapter) attachCitations(body map[string]any, citations *citationState, toolCalls *toolCallLog, eventFlags tinfoilEventFlags) {
	attachChatOutput(body, citations, toolCalls, eventFlags)
}

func (a *chatLoopAdapter) buildInitialRequest() map[string]any {
	reqBody := cloneJSONMap(a.body)
	delete(reqBody, "web_search_options")
	delete(reqBody, "code_execution_options")
	delete(reqBody, "filters")
	delete(reqBody, "stream_options")
	delete(reqBody, "pii_check_options")
	delete(reqBody, "prompt_injection_check_options")
	stripRouterOwnedIncludes(reqBody)
	reqBody["stream"] = false
	applyParallelToolCallsPolicy(reqBody)
	reqBody["tools"] = append(existingTools(reqBody["tools"]), chatTools(a.tools)...)
	reqBody["messages"] = prependChatPrompt(a.prompt, reqBody["messages"])

	if debugEnabled {
		ownedNames := make([]string, 0, len(a.ownedTools))
		for n := range a.ownedTools {
			ownedNames = append(ownedNames, n)
		}
		debugLogf("toolruntime:%s chat.start model=%s search_ctx=%q ownedTools=%v schemas=%d",
			a.tid, a.modelName, a.opts.searchContextSize, ownedNames, len(a.toolSchemas))
	}
	return reqBody
}

func (a *chatLoopAdapter) preIteration(reqBody map[string]any, iteration int) {
	if msgs, ok := reqBody["messages"].([]any); ok {
		if n := sanitizeAssistantToolCallsInMessages(msgs); n > 0 {
			debugLogf("toolruntime:%s chat.iter=%d history_sanitized count=%d", a.tid, iteration, n)
		}
	}
}

func (a *chatLoopAdapter) onUpstreamResponse(response *upstreamJSONResponse, iteration int, elapsed time.Duration) ([]toolCall, bool, any) {
	message, toolCalls := parseChatToolCalls(response.body)
	routerToolCalls, clientToolCalls := splitToolCalls(a.ownedTools, toolCalls)
	if debugEnabled {
		finishReason := ""
		content := ""
		if choices, _ := response.body["choices"].([]any); len(choices) > 0 {
			if ch, _ := choices[0].(map[string]any); ch != nil {
				finishReason = stringValue(ch["finish_reason"])
				if msg, _ := ch["message"].(map[string]any); msg != nil {
					content = stringValue(msg["content"])
				}
			}
		}
		toolNames := make([]string, 0, len(toolCalls))
		for _, c := range toolCalls {
			toolNames = append(toolNames, c.name)
		}
		debugLogf("toolruntime:%s chat.iter=%d upstream.response elapsed=%s finish=%q total_tool_calls=%d router_tool_calls=%d client_tool_calls=%d tool_names=%v content=%q",
			a.tid, iteration, elapsed, finishReason, len(toolCalls), len(routerToolCalls), len(clientToolCalls), toolNames, debugPreview(content, 240))
	}
	return routerToolCalls, len(clientToolCalls) > 0, message
}

// stripRouterToolCallsFromResponse keeps only the client-owned
// tool_calls on the assistant message and normalizes finish_reason to
// "tool_calls" so SDKs that gate on it collect tool outputs and
// re-post instead of treating the response as final.
func (a *chatLoopAdapter) stripRouterToolCallsFromResponse(response *upstreamJSONResponse) {
	if response == nil || response.body == nil {
		return
	}
	choices, _ := response.body["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return
	}
	rawCalls, _ := message["tool_calls"].([]any)
	if len(rawCalls) == 0 {
		return
	}
	filtered := make([]any, 0, len(rawCalls))
	for _, raw := range rawCalls {
		call, _ := raw.(map[string]any)
		if call == nil {
			filtered = append(filtered, raw)
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name := ""
		if fn != nil {
			name = stringValue(fn["name"])
		}
		if _, ok := a.ownedTools[name]; ok {
			continue
		}
		filtered = append(filtered, raw)
	}
	message["tool_calls"] = filtered
	if len(filtered) == 0 {
		delete(message, "tool_calls")
	}
	choice["finish_reason"] = "tool_calls"
}

func (a *chatLoopAdapter) applyToolOutputs(reqBody map[string]any, state any, outputs []toolOutput) map[string]any {
	messages, _ := reqBody["messages"].([]any)
	if assistant, ok := state.(map[string]any); ok && assistant != nil {
		messages = append(messages, assistant)
	}
	for _, o := range outputs {
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": o.callID,
			"content":      o.output,
		})
	}
	reqBody["messages"] = messages
	return reqBody
}

func (a *chatLoopAdapter) forcedFinalRequest(reqBody map[string]any) map[string]any {
	return forcedFinalChatRequest(reqBody)
}

// responsesLoopAdapter drives /v1/responses: it owns the accumulated input
// list that threads every prior output back into each subsequent turn, plus
// the `include` opt-in for `action.sources` on web_search_call items.
type responsesLoopAdapter struct {
	body             map[string]any
	prompt           *mcp.GetPromptResult
	tools            []*mcp.Tool
	ownedTools       map[string]struct{}
	base             map[string]any
	accumulatedInput []any
	opts             webSearchOptions
	toolSchemas      map[string]*jsonschema.Schema
}

func newResponsesLoopAdapter(body map[string]any, prompt *mcp.GetPromptResult, tools []*mcp.Tool, ownedTools map[string]struct{}) *responsesLoopAdapter {
	return &responsesLoopAdapter{
		body:        body,
		prompt:      prompt,
		tools:       tools,
		ownedTools:  ownedTools,
		opts:        parseResponsesWebSearchOptions(body),
		toolSchemas: schemaLookup(tools),
	}
}

func (a *responsesLoopAdapter) upstreamPath() string { return "/v1/responses" }
func (a *responsesLoopAdapter) traceID() string      { return "" }
func (a *responsesLoopAdapter) tracePhase(iteration int) string {
	return fmt.Sprintf("responses.iter=%d", iteration)
}
func (a *responsesLoopAdapter) includeActionSources() bool             { return a.opts.includeActionSources }
func (a *responsesLoopAdapter) options() webSearchOptions              { return a.opts }
func (a *responsesLoopAdapter) schemas() map[string]*jsonschema.Schema { return a.toolSchemas }
func (a *responsesLoopAdapter) preIteration(map[string]any, int)       {}

func (a *responsesLoopAdapter) applyUsage(response *upstreamJSONResponse, usage *tokencount.Usage) {
	if response == nil || response.body == nil || usage == nil {
		return
	}
	response.body["usage"] = map[string]any{
		"input_tokens":  usage.PromptTokens,
		"output_tokens": usage.CompletionTokens,
		"total_tokens":  usage.TotalTokens,
	}
}

func (a *responsesLoopAdapter) attachCitations(body map[string]any, citations *citationState, toolCalls *toolCallLog, _ tinfoilEventFlags) {
	attachResponsesOutput(body, citations, toolCalls, a.opts.includeActionSources)
}

func (a *responsesLoopAdapter) buildInitialRequest() map[string]any {
	base := cloneJSONMap(a.body)
	base["stream"] = false
	applyParallelToolCallsPolicy(base)
	delete(base, "stream_options")
	delete(base, "pii_check_options")
	delete(base, "prompt_injection_check_options")
	stripRouterOwnedIncludes(base)
	base["tools"] = replaceRouterOwnedResponsesTools(base["tools"], responseTools(a.tools))
	base["input"] = prependResponsesPrompt(a.prompt, base["input"])
	a.base = base
	a.accumulatedInput, _ = base["input"].([]any)
	return base
}

func (a *responsesLoopAdapter) onUpstreamResponse(response *upstreamJSONResponse, _ int, _ time.Duration) ([]toolCall, bool, any) {
	routerToolCalls, clientToolCalls := splitToolCalls(a.ownedTools, parseResponsesToolCalls(response.body))
	outputItems, _ := response.body["output"].([]any)
	return routerToolCalls, len(clientToolCalls) > 0, outputItems
}

// stripRouterToolCallsFromResponse drops router-owned function_call
// items from the output array; attachResponsesOutput prepends the
// matching web_search_call items that replace them.
func (a *responsesLoopAdapter) stripRouterToolCallsFromResponse(response *upstreamJSONResponse) {
	if response == nil || response.body == nil {
		return
	}
	outputItems, _ := response.body["output"].([]any)
	if len(outputItems) == 0 {
		return
	}
	filtered := make([]any, 0, len(outputItems))
	for _, rawItem := range outputItems {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			filtered = append(filtered, rawItem)
			continue
		}
		itemType := stringValue(item["type"])
		if itemType == "function_call" || itemType == "mcp_call" {
			if _, ok := a.ownedTools[stringValue(item["name"])]; ok {
				continue
			}
		}
		filtered = append(filtered, rawItem)
	}
	response.body["output"] = filtered
}

func (a *responsesLoopAdapter) applyToolOutputs(_ map[string]any, state any, outputs []toolOutput) map[string]any {
	outputItems, _ := state.([]any)
	a.accumulatedInput = append(a.accumulatedInput, normalizeResponsesOutputItems(outputItems)...)
	for _, o := range outputs {
		a.accumulatedInput = append(a.accumulatedInput, map[string]any{
			"type":    "function_call_output",
			"call_id": o.callID,
			"output":  o.output,
		})
	}
	reqBody := cloneJSONMap(a.base)
	reqBody["input"] = a.accumulatedInput
	return reqBody
}

func (a *responsesLoopAdapter) forcedFinalRequest(reqBody map[string]any) map[string]any {
	return forcedFinalResponsesRequest(reqBody)
}
