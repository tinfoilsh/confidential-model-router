# Tinfoil-Event Markers

## What gets produced

A single bash tool call recorded as:

```go
toolCalls.record(toolCallRecord{
    name:      "bash",
    arguments: map[string]any{"command": "ls"},
    output:    "file.txt",
})
```

produces this string from `tinfoilToolCallMarkersForRecords` (events.go:218):

```
\n<tinfoil-event>{"type":"tinfoil.tool_call","item_id":"tc_9f2a...","status":"in_progress","tool":{"name":"bash","arguments":{"command":"ls"}}}</tinfoil-event>\n
\n<tinfoil-event>{"type":"tinfoil.tool_call","item_id":"tc_9f2a...","status":"completed","tool":{"name":"bash","output":"file.txt"}}</tinfoil-event>\n
```

The `item_id` is `"tc_" + uuid.NewString()` --- generated on line 227 of events.go.
Both markers in the pair share the same UUID so clients can correlate them.

A search tool call recorded as:

```go
toolCalls.record(toolCallRecord{
    name:      "search",
    arguments: map[string]any{"query": "latest news"},
})
```

produces this string from `tinfoilEventMarkersForRecords` (events.go:148):

```
\n<tinfoil-event>{"type":"tinfoil.web_search_call","item_id":"ws_4b7c...","status":"in_progress","action":{"type":"search","query":"latest news"}}</tinfoil-event>\n
\n<tinfoil-event>{"type":"tinfoil.web_search_call","item_id":"ws_4b7c...","status":"completed","action":{"type":"search","query":"latest news"}}</tinfoil-event>\n
```

The `item_id` is `"ws_" + uuid.NewString()` --- generated on line 155 of events.go.

## How markers get into the response body

Two functions attach all tool-call output to the response body:

- `attachChatOutput` (events.go) --- Chat Completions surface
- `attachResponsesOutput` (events.go) --- Responses API surface

They are called from two adapter implementations:

1. `chatLoopAdapter.attachCitations` (tool_loop.go) -> calls `attachChatOutput`
2. `responsesLoopAdapter.attachCitations` (tool_loop.go) -> calls `attachResponsesOutput`

Both are called exactly once per request, at finalize time in `runToolLoop`.

### Chat path --- `attachChatOutput`

```go
func attachChatOutput(body map[string]any, citations *citationState, toolCalls *toolCallLog, eventFlags tinfoilEventFlags) {
    records := toolCalls.list()
    // for each choice:
    //   1. normalize fullwidth brackets in message.content
    //   2. compute url_citation annotations via citations.nestedAnnotationsFor
    //   3. build marker prefix --- one if-block per tool type:
    //      if eventFlags.webSearch:    prefix += tinfoilEventMarkersForRecords(records)
    //      if eventFlags.codeExecution: prefix += tinfoilToolCallMarkersForRecords(records)
    //   4. prepend prefix to content, shift annotation indices
    //   5. set message.content and message.annotations
}
```

After both marker families are enabled, a choice's content looks like:

```
[ws markers]\n[ce markers]\n[original assistant text]
```

### Responses path --- `attachResponsesOutput`

```go
func attachResponsesOutput(body map[string]any, citations *citationState, toolCalls *toolCallLog, includeActionSources bool) {
    // for each output_text item:
    //   1. normalize fullwidth brackets
    //   2. compute flat url_citation annotations
    //   3. attach annotations
    // then:
    //   4. buildWebSearchCallOutputItems(records, includeActionSources) -> web_search_call items
    //   5. buildCodeInterpreterCallOutputItems(records) -> code_interpreter_call items
    //   6. prepend both to body["output"]
}
```

Final `body["output"]` order: `[web_search_call items, code_interpreter_call items, message items]`

## Separation of concerns

Tool-call recording (`toolCallLog`) is separate from citation-source recording (`citationState`):

- `toolCallLog` accumulates `toolCallRecord` entries --- used to build markers and output items
- `citationState` accumulates `citationSource` entries --- used to match URLs in assistant text to url_citation annotations

Both are created at the start of `runToolLoop` and threaded through `executeRouterToolCall`.

## Why not idempotent

Both `tinfoilEventMarkersForRecords` and
`tinfoilToolCallMarkersForRecords` call `uuid.NewString()`
each time they run. Calling the same attach function twice would:

- Chat: prepend a second copy of the markers with different UUIDs
- Responses: prepend a second set of output items with different IDs

A sentinel (e.g. `body["_attached"] = true`) would work but these
functions are only called once per request at finalize time in
`runToolLoop`, so the guard would be dead code.
