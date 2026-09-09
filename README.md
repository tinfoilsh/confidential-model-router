# Confidential Inference Router

Tinfoil's confidential inference model router terminates TLS connections (optionally with EHBP), inspects the model name, and directs it to a verified secure inference enclave.

## Request bodies

The router accepts OpenAI-compatible bodies on `/v1/chat/completions` and `/v1/responses`. A few Tinfoil-specific top-level fields are recognized and stripped before the body is forwarded to the model enclave:

- `code_execution_options` — activates the code-execution tool profile. When code execution is requested, this object carries the per-request credentials (`accessToken`, `encryptionKey`, `containerAuthToken`).
- `web_search_options` — activates the web-search tool profile.
- `pii_check_options` — activates the PII safety check.
- `auto_model_options` — carries `{"intelligence": N}` for `model: "auto"` requests (see below).

## Auto model routing

`model: "auto"` asks the router to pick a concrete model and reasoning effort. The caller states how capable a model it wants as an intelligence level from 0 to 100, either in the body as `auto_model_options.intelligence` or in the `X-Tinfoil-Intelligence` header (the body wins; absent means 50). The implementation lives in `autoroute/`.

The catalog the router ranks over is every `type: "chat"` model in the control plane's `/v1/models` response that publishes an `intelligence` map. Each key of that map is a reasoning setting (`off`, `on`, `low`, `medium`, `high`) and each value is the model's Artificial Analysis Intelligence Index under that setting. How to fill those scores in for a new model is documented in the control plane repo at `config/README.md`.

Selection works as follows:

1. Expand every model into `(model, effort)` candidates, one per key in its `intelligence` map.
2. Normalize each raw score to a level: `round(score * 100 / maxScore)`, where `maxScore` is the highest score anywhere in the catalog. The strongest configuration is therefore always level 100 and everything else is relative to it, so levels shift when a stronger model is added.
3. If the request contains an image or file content part, drop text-only models.
4. Find the best fit: the smallest `|level - target|` over all candidates.
5. Every candidate within `FitTolerance` (3) levels of that best distance is "in band". In-band candidates sort first; within the band multimodal models win, then the nearer level, then the higher raw score, then the model name. Out-of-band candidates follow ordered by distance with the same tiebreaks.
6. Walk the ranked list and take the first model with a healthy enclave. If none is healthy the best fit is still returned so the normal serving path reports the outage.
7. Rewrite `body.model` to the chosen model and apply its published `reasoning_params` fragment for the chosen effort, replacing any `reasoning_effort` / `reasoning.effort` the client sent.

The tolerance band exists because two models that are one index point apart are not meaningfully different, and the more capable (multimodal) one should not lose every request to a rounding-sized gap. It also means a model does not need to land exactly on a client's slider stop to be selected. When adding or rescoring a model, check what each common target (the webapp sends 0, 20, 40, 60, 80, 100) resolves to; a score that sits more than `FitTolerance` levels away from every stop while another model sits on it will never be chosen except as a health fallback.

## Tool Calling

Client side tool calling is handled by the client. Server-side tools currently supported: **web search** and **code execution**.

To handle prompting, we add some instructions about how to call each tool in the system prompt, and we have a short description in each tool.

_vLLM handles putting the system prompt + the tool prompts together, using internal templates built for the specific models._
