# Saved search evidence

Chat clients opting into `X-Tinfoil-Events: web_search` receive an optional
`snippet` string on the existing `sources[]` metadata of completed search
and page-fetch markers. URLs and titles retain their existing format. A page
fetch carries only sources matching that page's URL. Older clients can ignore
the additional field; older responses without snippets remain valid.
Older clients may discard excerpts when rewriting a synced chat; replay then
falls back to the existing no-excerpt behavior until a new search runs.

Excerpts come from tool output, not the model's answer. Each snippet is at most
1,500 UTF-8 bytes, and each marker has at most 6,000 snippet bytes. Truncation
is marked with `[Excerpt truncated]`. These are saved excerpts, not complete
pages; the live model still receives the original tool output.

Clients persist excerpts on search and fetch timeline entries. On subsequent
turns they reconstruct paired assistant tool calls and tool-role results from
completed actions with excerpts. They never reconstruct evidence from prose,
citation annotations, failed actions, or URL-only legacy events. Request-local
call IDs pair each replayed call with its result; the replay does not execute
another search. Saved results contain exact source URLs and no generated
Harmony cursor numbers, which are local to a single router request.

Each assistant turn's replay is limited to 12,000 serialized UTF-16 code units,
with up to eight sources per action and 1,500 code units per excerpt. Recent
actions take precedence when the limit is reached. Replay is included in each
client's history-token estimate so archiving drops an entire assistant turn
and its paired evidence together. Source content remains tool-role data, never
a system instruction.

This change does not enforce search on every answer, add live tool-loop
compaction, or prove that a model's claims follow from its citations. Live-model
multi-turn evaluation is still needed before treating the reported behavior as
resolved.
