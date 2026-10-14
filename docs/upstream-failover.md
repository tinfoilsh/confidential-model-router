# Upstream rejection containment

For replayable, stateless JSON POSTs to `/v1/responses`, `/v1/chat/completions`,
and `/v1/completions`, an upstream HTTP 400 permits one attempt on a different
healthy replica before the router forwards a response. The alternate must have
unexpired attestation, a closed circuit, and no active overload rejection.
Selection respects the caller's reserved pool and permitted shared spillover.
The request body and credentials are preserved; admission, document conversion,
and router-side tools are not rerun.

If the alternate returns 2xx, its response is served and the rejecting enclave
is quarantined. This establishes a compatibility difference between replicas;
it does not establish which version's validation policy is preferable. A rollout
that intentionally tightens accepted inputs needs coordinated client changes.
If the alternate rejects the request or fails to connect, the original 400 is
preserved. No third attempt occurs. Ordinary invalid requests therefore do not
quarantine every healthy replica.

Only pre-response HTTP 400s trigger this behavior. The router does not replay
an accepted stream, tool-execution requests, or requests carrying backend-local
`previous_response_id` or `conversation` references. Errors after a stream starts
cannot be recovered by this mechanism. A valid baseline must remain available;
this cannot hide an incompatibility shared by every replica.

Compatibility quarantine is separate from the ordinary three-failure, 30-second
recovery circuit. It blocks automatic probes, cache-affinity choices, and degraded
last-resort routing. A successful unrelated request or attestation freshness
renewal does not clear it. Replacing the verified enclave identity, or removing
and re-adding the endpoint, installs a new breaker. State is local to each router
process and resets on router restart. Requests already dispatched may complete.

All upstream 4xx responses count in `router_proxy_failure_total` with bounded
HTTP status labels, and no longer count as proxy successes or clear ordinary
failure history. They do not otherwise trip the breaker. Client-visible HTTP
status contributes to request-duration outcomes for both streaming and
non-streaming requests. Quarantine publishes the existing breaker-open gauge
and logs the model and endpoint identities; it does not log request or response
content.

## Testing a new model release

1. Keep the candidate outside production routing. Verify attestation and exercise
   representative synthetic text, streaming, image, and tool-history requests,
   including integration defaults such as multimodal preprocessing options.
2. Introduce one candidate while retaining enough baseline capacity for fallback.
   Observe every router's breaker and upstream-failure metrics as real request
   types exercise the candidate. This change does not introduce traffic weights
   or a canary concurrency cap.
3. If quarantined, remove the candidate and repair it before reintroducing a new
   verified instance. Do not restart routers to clear the quarantine.
4. Expand the rollout only after the candidate handles representative traffic
   without new rejections. Keep a baseline until that evidence exists.
