**Mine:**

1. responses stream & chat stream are big boys. they rely on stream base.  
   They also rely on sse_reader, and tool_progress, iiuc. Same w/ events? All  
   these are streaming specific.
2. citation & citation stream both use citation_url. \
3. websearch options has no good fit. \
4. tool_loop & tool_call are sort of just used in main.go iiuc? These are  
   used for streaming & non streaming.
5. is there a normal chat & response? parity_test goes here sort of\.
6. coerce doesn't really belong anywehre - bit of a preprocessor. Same thing  
   w/ quirks iiuc.\
7. Registry is totally random iiuc. Maybe could be grouped w/ websearch  
   options & other tool specific thiungs? eh. Suppose routerToolName goes w/ it.

**Claude:**

group: Entry point  
 Files: runtime.go  
 Notes: Handle(), session setup, loop wrappers, upstream POST, response writing,  
 billing. The "main" of toolruntime.  
 ────────────────────────────────────────  
 Group: Prompt  
 Files: prompt.go  
 Notes: System prompt, citation instructions, forced-final, context message.
────────────────────────────────────────
Group: Tool loop (non-streaming)
Files: tool_loop.go, tool_call.go
Notes: Loop driver + adapters. tool_call.go has chatTools(), responseTools(),
splitToolCalls(), tool parsing. Used by both streaming and non-streaming.
────────────────────────────────────────
Group: Streaming
Files: chat_stream.go, responses_stream.go, stream_base.go, sse_reader.go,
tool_progress.go
Notes: The two big streamers embed stream_base. sse_reader parses SSE frames.
tool_progress is the emitter interface for surfacing search/fetch progress.
────────────────────────────────────────
Group: Events / output attachment
Files: events.go
Notes: attachChatOutput, attachResponsesOutput, tinfoil-event markers,
webSearchCallEvent, toolCallLog. Non-streaming finalize + the shared tool-call
recording used by both paths.
────────────────────────────────────────
Group: Citations
Files: citations.go, citation_stream.go, citation_url.go
Notes: The citation contract: format tool output → record sources → resolve/match
→
annotations. citation_stream.go is the streaming matcher. citation_url.go is URL

    normalization.

────────────────────────────────────────
Group: Tool plumbing
Files: session_registry.go, router_tool_names.go, websearch_options.go
Notes: Registry maps tool names to MCP sessions. Router tool names defines
router_search/router_fetch. Websearch options parses safety/depth/filter params.
────────────────────────────────────────
Group: Defensive/preprocessing
Files: quirks.go, coerce.go
Notes: Quirks strips control tokens, repairs malformed JSON. Coerce fixes argument

    types to match schemas.

────────────────────────────────────────
Group: Debug
Files: debug_disabled.go, debug_enabled.go
Notes: Build-tag-gated debug logging. debug_disabled.go is the default (no-ops).

**Going to split off citations.**
