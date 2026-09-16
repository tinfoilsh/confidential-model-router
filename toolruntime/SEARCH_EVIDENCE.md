# Saved search evidence

Chat clients opting into `X-Tinfoil-Events: web_search` receive an optional
`snippet` string on the existing `sources[]` metadata of completed search
and page-fetch markers. URLs and titles retain their existing format. A page
fetch carries only sources matching that page's URL. Older clients can ignore
the additional field; older responses without snippets remain valid.
Older clients may discard source text when rewriting a synced chat; replay then
falls back to the existing URL-only behavior until a new search runs.

The `snippet` field contains the full source content returned by the search or
fetch tool, not a summary or the model's answer. The router and clients do not
truncate it or select a subset of sources. Any limits already applied by the
underlying search/fetch service still apply.

Clients persist source text on search and fetch timeline entries. On subsequent
turns they reconstruct paired assistant tool calls and tool-role results from
completed actions with source text. They never reconstruct evidence from prose,
citation annotations, failed actions, or URL-only legacy events. Request-local
call IDs pair each replayed call with its result; the replay does not execute
another search. Saved results contain exact source URLs and no generated
Harmony cursor numbers, which are local to a single router request.

Actions and sources retain their original order. Replay is included in each
client's existing history-token estimate so archiving drops an entire assistant turn
and its paired evidence together. Source content remains tool-role data, never
a system instruction.

Full source text can consume the history budget sooner. Source compression,
such as boilerplate removal or model summaries, is a future optimization rather
than part of this change.

This change does not enforce search on every answer, add live tool-loop
compaction, or prove that a model's claims follow from its citations. Live-model
multi-turn evaluation is still needed before treating the reported behavior as
resolved.
