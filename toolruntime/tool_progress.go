package toolruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
)

// toolProgressEmitter abstracts the surface-specific way a streamer
// announces router-owned tool progress. Chat streams surface progress
// inline as `<tinfoil-event>` markers that ride inside content deltas;
// the Responses stream uses the spec-defined `response.output_item.added`
// / `response.web_search_call.*` / `response.output_item.done` events.
//
// The interface is deliberately minimal so executeToolWithProgress can
// drive both surfaces from one code path. Implementations capture any
// per-call state (e.g. the Responses `output_index`) on the value they
// return from open() and pass back into phase() / close() so the shared
// driver never needs to know which wire format is in flight.
type toolProgressEmitter interface {
	// open announces that a router-owned tool call has begun. The
	// returned handle is opaque to the driver and passed back into
	// phase / close so surface-specific state (e.g. the Responses
	// output_index) can flow through without the driver knowing
	// about it.
	open(id string, action map[string]any) toolProgressHandle

	// phase emits a spec-defined intermediate progress envelope for
	// the call. On the Chat surface (which has no spec intermediate
	// phases) this is a no-op.
	phase(handle toolProgressHandle, phase string)

	// close emits the terminal progress frame for the call. status
	// is one of "completed" / "failed" / "blocked"; reason is the
	// opaque router error code surfaced to clients on non-success.
	// sources carries the ordered {url, title} pairs this specific
	// call produced so clients can attribute citations to the search
	// that surfaced them. May be nil on non-search calls or error
	// terminations where no sources are available.
	close(handle toolProgressHandle, action map[string]any, status, reason string, sources []toolCallSource)
}

// toolProgressHandle is the opaque per-call handle returned by
// toolProgressEmitter.open. Chat ignores the outputIndex; Responses
// uses it to correlate subsequent phase/close envelopes with the
// matching output_item.added event.
type toolProgressHandle struct {
	id          string
	outputIndex int
}

// executeToolWithProgress runs a single router-owned tool call and
// surfaces progress via the supplied emitter. Search and fetch are the
// two tools that ship router-owned progress: search emits one
// in_progress/completed (or failed/blocked) pair; fetch emits one pair
// per URL so clients can correlate each open_page action with its
// terminal status. Every other tool call is delegated to the MCP session
// without any progress envelopes.
//
// Errors from callTool are returned verbatim; the caller turns them into
// the humanized payload the upstream model sees via humanizeToolArgError
// (matching the non-streaming contract in executeRouterToolCall).
func executeToolWithProgress(
	ctx context.Context,
	registry *sessionRegistry,
	citations *citationState,
	emitter toolProgressEmitter,
	call toolCall,
) (string, error) {
	session, ok := registry.sessionFor(call.name)
	if !ok {
		return "", fmt.Errorf("no MCP session registered for tool %q", call.name)
	}
	dispatchName := registry.dispatchName(call.name)
	switch {
	case isRouterSearchToolName(call.name):
		id := "ws_" + uuid.NewString()
		action := map[string]any{"type": "search", "query": stringValue(call.arguments["query"])}
		handle := emitter.open(id, action)
		emitter.phase(handle, "response.web_search_call.in_progress")
		emitter.phase(handle, "response.web_search_call.searching")
		output, err := callTool(ctx, session, dispatchName, call.arguments, citations)
		if err != nil {
			emitter.close(handle, action, failureStatusFor(err), publicToolErrorReason(call.name, err), nil)
			return "", err
		}
		sources := extractToolOutputSources(output)
		emitter.phase(handle, "response.web_search_call.completed")
		emitter.close(handle, action, "completed", "", sources)
		return output, nil
	case isRouterFetchToolName(call.name):
		urls := fetchArgumentURLs(call.arguments)
		if len(urls) == 0 {
			return callTool(ctx, session, dispatchName, call.arguments, citations)
		}
		handles := make([]toolProgressHandle, len(urls))
		actions := make([]map[string]any, len(urls))
		for i, url := range urls {
			id := "ws_" + uuid.NewString()
			actions[i] = map[string]any{"type": "open_page", "url": url}
			handles[i] = emitter.open(id, actions[i])
			emitter.phase(handles[i], "response.web_search_call.in_progress")
		}
		output, err := callTool(ctx, session, dispatchName, call.arguments, citations)
		if err != nil {
			reason := publicToolErrorReason(call.name, err)
			status := failureStatusFor(err)
			for i := range urls {
				emitter.close(handles[i], actions[i], status, reason, nil)
			}
			return "", err
		}
		for i := range urls {
			emitter.phase(handles[i], "response.web_search_call.completed")
			emitter.close(handles[i], actions[i], "completed", "", nil)
		}
		return output, nil
	default:
		return callTool(ctx, session, dispatchName, call.arguments, citations)
	}
}

// chatToolProgressEmitter surfaces router-owned tool progress on the
// Chat Completions streaming surface as `<tinfoil-event>` markers
// carried inside assistant content deltas. Intermediate spec phases
// (in_progress / searching / completed) are folded into the terminal
// marker because the Chat marker format only surfaces the final state;
// the opt-in marker contract is one in_progress + one terminal marker
// per call, matching what tinfoilEventMarkersForRecords emits on the
// non-streaming path.
type chatToolProgressEmitter struct {
	streamer *chatStreamer
}

func (c *chatToolProgressEmitter) open(id string, action map[string]any) toolProgressHandle {
	c.streamer.emitTinfoilEventMarker(id, "in_progress", action, "", nil)
	return toolProgressHandle{id: id}
}

func (c *chatToolProgressEmitter) phase(toolProgressHandle, string) {}

func (c *chatToolProgressEmitter) close(handle toolProgressHandle, action map[string]any, status, reason string, sources []toolCallSource) {
	c.streamer.emitTinfoilEventMarker(handle.id, status, action, reason, sources)
}

// responsesToolProgressEmitter surfaces router-owned tool progress on
// the Responses streaming surface as the spec-defined event envelopes
// OpenAI documents: response.output_item.added carrying a web_search_call
// item, followed by response.web_search_call.{in_progress,searching,
// completed} phase events, terminated by response.output_item.done.
type responsesToolProgressEmitter struct {
	streamer *responsesStreamer
}

func (r *responsesToolProgressEmitter) open(id string, action map[string]any) toolProgressHandle {
	outputIndex := r.streamer.openWebSearchCallItem(id, action)
	return toolProgressHandle{id: id, outputIndex: outputIndex}
}

func (r *responsesToolProgressEmitter) phase(handle toolProgressHandle, phase string) {
	r.streamer.emitToolCallPhase(phase, handle.id, handle.outputIndex)
}

func (r *responsesToolProgressEmitter) close(handle toolProgressHandle, action map[string]any, status, reason string, _ []toolCallSource) {
	r.streamer.closeWebSearchCallItem(handle.id, handle.outputIndex, action, status, reason)
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
	citations *citationState,
	toolCalls *toolCallLog,
	executor func(ctx context.Context, call toolCall) (string, error),
	tracePhase, traceID string,
) string {
	applyWebSearchOptionsToToolCall(call.name, call.arguments, opts)
	sanitizeToolCallArguments(call.name, call.arguments)
	coerceArgumentsToSchema(call.name, call.arguments, toolSchemas)

	if traceID != "" {
		debugLogf("toolruntime:%s %s tool.call name=%s args=%s", traceID, tracePhase, call.name, debugPreview(call.arguments, 400))
	}
	tstart := time.Now()
	output, err := executor(ctx, call)
	record := toolCallRecord{
		name:      call.name,
		arguments: call.arguments,
	}
	if err != nil {
		if traceID != "" {
			debugLogf("toolruntime:%s %s tool.error name=%s elapsed=%s err=%v", traceID, tracePhase, call.name, time.Since(tstart), err)
		}
		output = humanizeToolArgError(call.name, err, call.arguments)
		record.errorReason = publicToolErrorReason(call.name, err)
	} else {
		record.resultURLs = extractToolOutputURLs(output)
		record.resultSources = extractToolOutputSources(output)
		if traceID != "" {
			debugLogf("toolruntime:%s %s tool.result name=%s elapsed=%s output_len=%d urls=%v preview=%q",
				traceID, tracePhase, call.name, time.Since(tstart), len(output), record.resultURLs, debugPreview(output, 400))
		}
	}
	toolCalls.record(record)
	return output
}
