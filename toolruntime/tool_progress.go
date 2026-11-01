package toolruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinfoilsh/confidential-model-router/toolruntime/citations"
)

// toolProgressEmitter abstracts the surface-specific way a streamer
// announces router-owned tool progress. Chat streams surface progress
// inline as `<tinfoil-event>` markers that ride inside content deltas;
// the Responses stream uses the spec-defined `response.output_item.added`
// / `response.web_search_call.*` / `response.output_item.done` events.
//
// The interface is deliberately minimal so executeToolWithProgress can
// drive both surfaces from one code path. The handle returned by open()
// carries opaque per-call state (e.g. the Responses output_index) so the
// shared driver never needs to know which surface is in flight.
type toolProgressEmitter interface {
	// open announces that a router-owned tool call has begun. details
	// is the public-facing web search action.
	open(id string, details map[string]any) toolProgressHandle

	// phase emits a spec-defined intermediate progress envelope.
	// On the Chat surface this is a no-op (Chat has no spec phases).
	phase(handle toolProgressHandle, phase string)

	// close emits the terminal progress frame for a tool call.
	// result carries the source metadata and PII report the search
	// produced. Each implementation picks the fields it needs and
	// ignores the rest.
	close(handle toolProgressHandle, toolName string, details map[string]any, result toolProgressResult, status, reason string)
}

// toolProgressResult carries the terminal metadata for a web search
// tool call.
type toolProgressResult struct {
	sources []toolCallSource // web search: source metadata
	pii     *piiCheckResult  // search: the server's PII masking report
}

// toolExecution is what a successful router-owned tool call hands back
// to the loop: the text the upstream model sees plus the metadata the
// loop records on the toolCallRecord for client-facing progress items.
type toolExecution struct {
	output     string
	sources    []toolCallSource
	pii        *piiCheckResult
	piiBilling serviceBilling
}

// toolProgressHandle is the opaque per-call handle returned by
// toolProgressEmitter.open. Chat ignores the outputIndex; Responses
// uses it to correlate subsequent phase/close envelopes with the
// matching output_item.added event.
type toolProgressHandle struct {
	id          string
	outputIndex int
}

// toolPhaseConfig describes the progress phases a tool kind emits
// around its MCP call. The idPrefix seeds the item ID; preCallPhases
// are emitted before calling the tool; completedPhase is emitted
// after a successful call, before close.
type toolPhaseConfig struct {
	idPrefix       string
	preCallPhases  []string
	completedPhase string
}

// toolPhaseConfigs maps tool names to their progress phase
// configuration. Adding a new tool kind is a one-line addition.
var toolPhaseConfigs = map[string]toolPhaseConfig{
	"search": {
		idPrefix:       "ws_",
		preCallPhases:  []string{"response.web_search_call.in_progress", "response.web_search_call.searching"},
		completedPhase: "response.web_search_call.completed",
	},
	"fetch": {
		idPrefix:       "ws_",
		preCallPhases:  []string{"response.web_search_call.in_progress"},
		completedPhase: "response.web_search_call.completed",
	},
}

func toolPhasesFor(name string) toolPhaseConfig {
	return toolPhaseConfigs[dispatchRouterToolName(name)]
}

// executeToolWithProgress runs a single router-owned tool call and
// surfaces progress via the supplied emitter. Search follows the
// single-handle pattern (open, phases, call, close); fetch opens one
// handle per URL but makes a single MCP call for all of them. Every
// other tool call is delegated to the MCP session without any
// progress envelopes.
//
// Errors from callTool are returned verbatim; the caller turns them
// into the humanized payload the upstream model sees via
// humanizeToolArgError.
func executeToolWithProgress(
	ctx context.Context,
	registry *sessionRegistry,
	state *citations.State,
	emitter toolProgressEmitter,
	call toolCall,
) (toolExecution, error) {
	session, ok := registry.sessionFor(call.name)
	if !ok {
		return toolExecution{}, fmt.Errorf("no MCP session registered for tool %q", call.name)
	}
	dispatchName := registry.dispatchName(call.name)
	phases := toolPhasesFor(call.name)

	switch {
	case isRouterSearchToolName(call.name):
		details := map[string]any{"type": "search", "query": stringValue(call.arguments["query"])}
		return executeSingleToolWithProgress(ctx, session, state, emitter, call, dispatchName, phases, details)

	case isRouterFetchToolName(call.name):
		return executeFetchWithProgress(ctx, session, state, emitter, call, dispatchName, phases)

	default:
		output, structured, err := callTool(ctx, session, dispatchName, call.arguments)
		if err != nil {
			return toolExecution{}, err
		}
		return toolExecution{output: applyStructuredFormat(call.name, output, structured, state)}, nil
	}
}

// executeSingleToolWithProgress handles the single-handle pattern used
// by search: open one handle, emit pre-call phases, call the tool, emit
// completed phase on success, then close.
func executeSingleToolWithProgress(
	ctx context.Context,
	session *mcp.ClientSession,
	state *citations.State,
	emitter toolProgressEmitter,
	call toolCall,
	dispatchName string,
	phases toolPhaseConfig,
	details map[string]any,
) (toolExecution, error) {
	id := phases.idPrefix + uuid.NewString()
	handle := emitter.open(id, details)
	for _, phase := range phases.preCallPhases {
		emitter.phase(handle, phase)
	}

	output, structured, err := callTool(ctx, session, dispatchName, call.arguments)
	billing := piiBillingFromStructured(call.name, structured, err)
	if err != nil {
		emitter.close(handle, call.name, details, toolProgressResult{}, "failed", publicToolErrorReason(call.name, err))
		return toolExecution{piiBilling: billing}, err
	}
	execution := toolExecution{
		output:     applyStructuredFormat(call.name, output, structured, state),
		pii:        piiCheckResultFromStructured(call.name, structured),
		piiBilling: billing,
	}
	execution.sources = toolCallSourcesForResult(call.name, structured, execution.output)

	emitter.phase(handle, phases.completedPhase)
	result := toolProgressResult{
		sources: execution.sources,
		pii:     execution.pii,
	}
	emitter.close(handle, call.name, details, result, "completed", "")
	return execution, nil
}

// executeFetchWithProgress handles the fetch-specific multi-handle
// pattern: one handle per URL, but a single MCP call for all of them.
func executeFetchWithProgress(
	ctx context.Context,
	session *mcp.ClientSession,
	state *citations.State,
	emitter toolProgressEmitter,
	call toolCall,
	dispatchName string,
	phases toolPhaseConfig,
) (toolExecution, error) {
	urls := fetchArgumentURLs(call.arguments)
	if len(urls) == 0 {
		output, structured, err := callTool(ctx, session, dispatchName, call.arguments)
		if err != nil {
			return toolExecution{}, err
		}
		return toolExecution{output: applyStructuredFormat(call.name, output, structured, state)}, nil
	}

	handles := make([]toolProgressHandle, len(urls))
	details := make([]map[string]any, len(urls))
	for i, url := range urls {
		id := phases.idPrefix + uuid.NewString()
		details[i] = map[string]any{"type": "open_page", "url": url}
		handles[i] = emitter.open(id, details[i])
		for _, phase := range phases.preCallPhases {
			emitter.phase(handles[i], phase)
		}
	}

	output, structured, err := callTool(ctx, session, dispatchName, call.arguments)
	if err != nil {
		reason := publicToolErrorReason(call.name, err)
		for i := range urls {
			emitter.close(handles[i], call.name, details[i], toolProgressResult{}, "failed", reason)
		}
		return toolExecution{}, err
	}
	execution := toolExecution{output: applyStructuredFormat(call.name, output, structured, state)}
	execution.sources = toolCallSourcesForResult(call.name, structured, execution.output)
	for i := range urls {
		emitter.phase(handles[i], phases.completedPhase)
		emitter.close(handles[i], call.name, details[i], toolProgressResult{sources: sourcesForURL(execution.sources, urls[i])}, "completed", "")
	}
	return execution, nil
}

// chatToolProgressEmitter surfaces router-owned tool progress on the
// Chat Completions streaming surface as `<tinfoil-event>` markers
// carried inside assistant content deltas. Intermediate spec phases
// are no-ops because the Chat marker format only surfaces the final
// state; the contract is one in_progress + one terminal marker per
// call, matching tinfoilEventMarkersForRecords on the non-streaming path.
type chatToolProgressEmitter struct {
	streamer *chatStreamer
}

func (c *chatToolProgressEmitter) open(id string, details map[string]any) toolProgressHandle {
	c.streamer.emitTinfoilEventMarker(id, "in_progress", details, "", nil, nil)
	return toolProgressHandle{id: id}
}

func (c *chatToolProgressEmitter) phase(toolProgressHandle, string) {}

func (c *chatToolProgressEmitter) close(handle toolProgressHandle, _ string, details map[string]any, result toolProgressResult, status, reason string) {
	c.streamer.emitTinfoilEventMarker(handle.id, status, details, reason, result.sources, result.pii)
}

// responsesToolProgressEmitter surfaces router-owned tool progress on
// the Responses streaming surface as the spec-defined event envelopes
// OpenAI documents: response.output_item.added, phase events, and
// response.output_item.done.
type responsesToolProgressEmitter struct {
	streamer *responsesStreamer
}

func (r *responsesToolProgressEmitter) open(id string, details map[string]any) toolProgressHandle {
	outputIndex := r.streamer.openWebSearchCallItem(id, details)
	return toolProgressHandle{id: id, outputIndex: outputIndex}
}

func (r *responsesToolProgressEmitter) phase(handle toolProgressHandle, phase string) {
	r.streamer.emitToolCallPhase(phase, handle.id, handle.outputIndex)
}

func (r *responsesToolProgressEmitter) close(handle toolProgressHandle, toolName string, details map[string]any, result toolProgressResult, status, reason string) {
	action := details
	if isRouterSearchToolName(toolName) && r.streamer.includeActionSources {
		if sources := toActionSources(result.sources); len(sources) > 0 {
			action = make(map[string]any, len(details)+1)
			for key, value := range details {
				action[key] = value
			}
			action["sources"] = sources
		}
	}
	r.streamer.closeWebSearchCallItem(handle.id, handle.outputIndex, action, status, reason, result.pii)
}

// resolveStreamingRouterToolCall runs the per-iteration argument-coercion,
// tool-execution, and citation-recording sequence shared by both streaming
// surfaces. It mutates call.arguments in place with forwarded web-search
// options and schema coercion, invokes executor (which wraps the MCP call
// plus surface-specific progress emission), records the outcome on
// citations, and returns the text payload to hand back to the upstream
// model -- either the tool's output on success or a humanized error
// message on failure. This mirrors the non-streaming executeRouterToolCall
// contract with the difference that the executor is injected so each
// streamer can layer its own progress events around the MCP call.
//
// When traceID is non-empty the helper emits the same structured
// `tool.call` / `tool.error` / `tool.result` debug lines the non-streaming
// path produces, keyed off tracePhase (e.g. `chatstream.iter=N` or
// `responses.iter=N`). Streamers that do not thread a trace id (the
// Responses streamer does not today) pass "" and get silent execution.
func resolveStreamingRouterToolCall(
	ctx context.Context,
	call toolCall,
	opts webSearchOptions,
	toolSchemas map[string]*jsonschema.Schema,
	toolCalls *toolCallLog,
	executor func(ctx context.Context, call toolCall) (toolExecution, error),
	tracePhase, traceID string,
) string {
	applyWebSearchOptionsToToolCall(call.name, call.arguments, opts)
	sanitizeToolCallArguments(call.name, call.arguments)
	coerceArgumentsToSchema(call.name, call.arguments, toolSchemas)

	if traceID != "" {
		debugLogf("toolruntime:%s %s tool.call name=%s args=%s", traceID, tracePhase, call.name, debugPreview(call.arguments, 400))
	}
	tstart := time.Now()
	execution, err := executor(ctx, call)
	output := execution.output
	record := toolCallRecord{
		name:       call.name,
		arguments:  call.arguments,
		piiBilling: execution.piiBilling,
	}
	if err != nil {
		if traceID != "" {
			debugLogf("toolruntime:%s %s tool.error name=%s elapsed=%s err=%v", traceID, tracePhase, call.name, time.Since(tstart), err)
		}
		output = humanizeToolArgError(call.name, err, call.arguments)
		record.errorReason = publicToolErrorReason(call.name, err)
	} else {
		record.resultSources = execution.sources
		record.resultURLs = toolCallSourceURLs(record.resultSources)
		record.pii = execution.pii
		if traceID != "" {
			debugLogf("toolruntime:%s %s tool.result name=%s elapsed=%s output_len=%d urls=%v preview=%q",
				traceID, tracePhase, call.name, time.Since(tstart), len(output), record.resultURLs, debugPreview(output, 400))
		}
	}
	toolCalls.record(record)
	return output
}
