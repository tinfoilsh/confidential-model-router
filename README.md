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

The router has no model-specific preferences or tuning of its own: how well a candidate fits a level comes entirely from the scores the control plane publishes, and the router only adds the generic rules below (request content, enclave health, deterministic tie-breaks). Changing which model serves a given level is therefore a control plane config change, not a router change. How to score a model is documented in the control plane repo at `config/README.md`.

The catalog the router ranks over is every `type: "chat"` model in the control plane's `/v1/models` response that publishes an `intelligence` map. Each key of that map is a reasoning setting (`off`, `on`, `low`, `medium`, `high`) and each value is the model's Artificial Analysis Intelligence Index under that setting.

Selection works as follows:

1. Expand every model into `(model, effort)` candidates, one per key in its `intelligence` map.
2. Normalize each raw score to a level: `round(score * 100 / maxScore)`, where `maxScore` is the highest score anywhere in the catalog. The strongest configuration is therefore always level 100 and everything else is relative to it, so levels shift when a stronger model is added.
3. If the request contains an image or file content part, drop text-only models.
4. Sort candidates by `|level - target|`, nearest first. Exact ties are broken by multimodal first, then the higher raw score, then the model name, then the effort key, so the order is deterministic.
5. Walk the ranked list and take the first model with a healthy enclave. If none is healthy the best fit is still returned so the normal serving path reports the outage.
6. Rewrite `body.model` to the chosen model and apply its published `reasoning_params` fragment for the chosen effort (with `$EFFORT` replaced by the model's native value from `effort_map`), replacing any `reasoning_effort` / `reasoning.effort` the client sent.

Because only the nearest candidate wins, a model whose level is one point further from the target than another's will never be chosen for text requests except as a health fallback; the tie-breaks only apply between candidates at the same level. If two models should share a level with the multimodal one preferred, the control plane must publish equal scores for them; the router does not apply a tolerance of its own.

## Request admission

Opaque API keys make one uncached `POST /api/shim/route-context` call with the resolved served model for each external inference request, before file conversion or tool execution. The control plane owns per-account/model shared RPM and lazy TPM policy: `rejected` returns 429 with a request/token-specific message and `Retry-After`, `demote` applies priority 1 on supported JSON endpoints, and `exempt` preserves configured priority and bypasses backend overload shedding. Configured-priority callers also retain their overload exemption. Credential/payment failures retain 401/403/402. A lookup the control plane cannot answer (timeout, transport error, non-denial status, or an unreadable decision) admits the request to the shared pool with default priority and no quota decision, counted in `router_route_context_lookup_failures_total`: the enclave still authenticates every request, so a control plane outage degrades quota enforcement rather than inference. Lookups have a 500 ms timeout and are not retried.

Bearers shaped as typed `at+jwt` access tokens skip route-context and have no local RPM limits. This classification bypasses only quota admission, not authentication. Billing, first-party safeguards, and delegated service credentials still apply. Internal tool/file dispatches reuse the request context without another admission. Input-token counting uses only the model-less metadata lookup (also skipped for JWTs), never rate admission. A legacy `rate_limit` block in runtime YAML is ignored, so old and new routers can share one config during a rolling deploy; backend overload protection remains local.

The supported inference ingress is the router. Its first-hop shim authenticates only `/metrics`, as configured in `tinfoil-config.yml`; downstream model and tool shims verify inference JWT signatures and claims before serving. A forged JWT can match the router's classifier and skip quota lookup but must still be rejected by the downstream shim without obtaining inference. Classification does not establish that a token is already verified. Direct backend access is not a supported ingress and its quota-bypass implications are a separate deployment concern. Client-supplied request IDs never bypass admission. Only known inference JSON schemas are rewritten for priority; multipart, compressed, and opaque bodies on other subdomain endpoints retain their wire representation.

Decisions are validated before use against the contract table in the control plane's `docs/model-rate-limits.md`: `rejected` requires reason `requests` or `tokens` and a positive retry delay, `demote` requires reason `requests`, and `allowed`/`exempt` require an empty or omitted reason. Optional quota counts do not determine the decision locally. `router_ratelimit_rejections_total{model}` remains available for compatibility; `router_ratelimit_rejections_by_reason_total{model,reason}` distinguishes RPM (`requests`) from TPM (`tokens`) rejections using only those two reason labels.

Nonstreaming tool loops report the actual usage from fully consumed, successful model turns once, even if a later turn fails or is canceled. Unconsumed or failed-turn tokens are not inferred. These completed tokens remain accountable for both lazy TPM and billing; the original failure still reaches the caller.

Admission client/config tests run with `go test ./...`. The real-handler orchestration and billing tests use local TLS backends, an MCP server, and a usage ingestion fixture, with fixture-only manager access behind the existing `localharness` build tag: `go test -tags localharness . -run 'TestAdmissionHandler|TestNonstreamToolBilling'`. They cover admission ordering, route types, reservation reuse, priority/overload behavior, downstream JWT rejection propagation, delegation, and completed-turn billing on success/error/cancellation without remote attestation services. The JWT fixtures test forwarding and rejection propagation, not cryptographic verification by the downstream shim implementation.

## Acceptable use safeguards

When `SAFEGUARDS_URL` is set, the router submits each completed first-party chat turn to the safeguards sidecar declared in `tinfoil-config.yml`. Only requests authenticated with a first-party chat access token on `/v1/chat/completions` or `/v1/responses` are considered; API-key traffic is never submitted. The router reassembles the assistant's reply from the response it wrote to the client, appends it to the request history, and hands the conversation off asynchronously so the inference response is never delayed or failed by the sidecar. The optional `X-Tinfoil-Conversation-Id` header lets the client identify a chat so a continued conversation is only ever counted once; it is stripped before the request reaches the model.

The sidecar classifies the transcript and reports confirmed violations to the control plane, which verifies the user's own token and applies the warning and ban thresholds. See the `confidential-safeguards` repo for the classification pipeline.

## Tool Calling

Client side tool calling is handled by the client. Server-side tools currently supported: **web search** and **code execution**.

To handle prompting, we add some instructions about how to call each tool in the system prompt, and we have a short description in each tool.

_vLLM handles putting the system prompt + the tool prompts together, using internal templates built for the specific models._
