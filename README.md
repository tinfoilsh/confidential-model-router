# Confidential Inference Router

Tinfoil's confidential inference model router terminates TLS connections (optionally with EHBP), inspects the model name, and directs it to a verified secure inference enclave.

## Downstream V3 attestation

Each backend must pass nonce-bound Go V3 verification against the repository pinned in the initial router configuration. The backend supplies code and platform endorsements, freshness witnesses and CPU evidence. There is no V2 fallback and no GitHub Latest lookup: different freshly endorsed releases may coexist in one model pool. Releases remain eligible only while both their code and platform freshness proofs are valid; accepting an older release does not cause its witnesses to be renewed.

New backends are verified before admission. Healthy backends are then fully re-attested every **24 hours**, or sooner at **one hour before their freshness deadline**. The existing worker still checks configuration, retries due/failed attestations and performs lightweight TLS-key probes every five minutes by default (`REFRESH_INTERVAL` / `-r`). A changed TLS key triggers full V3 verification in that cycle; the probe itself never authorizes a key. At most four backend checks run concurrently. A failed configuration fetch still checks the last known targets. Network and verification work runs outside routing locks and off the inference request path.

- Renewing an unchanged key updates its authenticated deadline without resetting the proxy, circuit breaker, metrics or active streams.
- Temporary attestation fetch failures retain the previous result only until its original freshness deadline. Invalid evidence immediately removes the backend.
- Expiry is checked during selection and before each new downstream HTTP exchange, including cached MCP clients and file uploads. Recovery probes and health fallbacks cannot override expiry. Already-started streams may finish.
- Every downstream service connection remains pinned to the V3-endorsed TLS key. Key rotation replaces the backend and retires old clients.

`/.well-known/tinfoil-proxy` advertises only currently verified endpoints. Release tag, digest, code measurement and `freshness_expires_at` are reported per endpoint, not once per model. `proxyctl list` likewise reports one row per endpoint.

For an opt-in read-only live check of router admission plus pinned HTTPS (no inference traffic):

```sh
TINFOIL_V3_AUDIT_CONFIG=/path/to/model-config.yml go test ./manager -run '^TestLiveV3Backends$' -count=1 -v
```

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

## Acceptable use safeguards

When `SAFEGUARDS_URL` is set, the router submits each completed first-party chat turn to the safeguards sidecar declared in `tinfoil-config.yml`. Only requests authenticated with a first-party chat access token on `/v1/chat/completions` or `/v1/responses` are considered; API-key traffic is never submitted. The router reassembles the assistant's reply from the response it wrote to the client, appends it to the request history, and hands the conversation off asynchronously so the inference response is never delayed or failed by the sidecar. The optional `X-Tinfoil-Conversation-Id` header lets the client identify a chat so a continued conversation is only ever counted once; it is stripped before the request reaches the model.

The sidecar classifies the transcript and reports confirmed violations to the control plane, which verifies the user's own token and applies the warning and ban thresholds. See the `confidential-safeguards` repo for the classification pipeline.

## Tool Calling

Client side tool calling is handled by the client. Server-side tools currently supported: **web search** and **code execution**.

To handle prompting, we add some instructions about how to call each tool in the system prompt, and we have a short description in each tool.

_vLLM handles putting the system prompt + the tool prompts together, using internal templates built for the specific models._
