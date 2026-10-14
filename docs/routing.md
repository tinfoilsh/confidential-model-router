# Pool routing and overload

Every request uses the same tier selector. The model's `overload` policy
applies to public requests; a reservation's block replaces it for that
reservation's callers. An omitted block inherits the model policy; an empty
block disables load guards. The caller's policy follows spillover to shared
capacity. Multiple reservation entries for one organization must agree on
that policy.

| Tier | Meaning under the caller's policy |
| --- | --- |
| 0 | No waiting requests; running below the configured limit, if enabled |
| 1 | Not tier 0, but the queue guard has not tripped |
| 2 | Queue reached `max_requests_waiting` and has not recovered |

Reserved requests prefer **reserved 0 → shared 0 → reserved 1 → shared 1**.
Cache affinity breaks ties within the selected pool and tier, subject to
`cache_route.max_inflight_delta`. Public traffic uses shared hosts only.
Without reservations, all hosts form one pool.

`max_requests_running` is a soft preference, never a rejection threshold.
A nonpositive value disables that threshold. A tripped queue guard clears
at or below `clear_requests_waiting`, defaulting to half the waiting limit,
rounded down. A waiting limit of 1 clears at zero; a nonpositive waiting
limit disables queue rejection. Routing and telemetry share recovery state
for each configured pair of queue thresholds.

When all usable candidates are tier 2, ordinary requests receive **HTTP 503**
with `Retry-After` from the caller's policy (default: one minute). Configured
priority requests and internal dispatches use the same selection but serve
through overload.

vLLM metrics are polled at a nominal one-second interval, including on shared
hosts needed for spillover. Slow scrapes reduce that cadence. These sampled
thresholds are not atomic concurrency caps. For an
enabled policy, missing/incomplete samples or samples older than 15 seconds
are **unknown**, never tier 0. Known tier 0 and tier 1 candidates win first;
otherwise unknown candidates fail open, reserved before shared. Disabled
policies impose no load preference or rejection and do not require metrics.

Expired attestations are excluded. Due circuit-breaker recovery probes bypass
load preferences when their pool is considered and must be dispatched once
claimed. When no closed backend exists, selection allows a degraded fallback.

`router_reserved_pool_serves_total{pool="spilled"}` counts spillover dispatches.
Backend overload gauges describe the host's own pool policy, not every caller's
tier. Cache affinity estimates reuse; it does not verify KV-cache residency.

Illustrative configuration, not workload recommendations:

```yaml
models:
  gpt-oss-120b:
    repo: example/model
    enclaves: [reserved.example, shared.example]
    overload: {max_requests_running: 6, max_requests_waiting: 12}
    reservations:
      - org_ids: [example-org]
        enclaves: [reserved.example]
        overload: {max_requests_running: 3, max_requests_waiting: 1}
```

A shared host with four running requests and no queue is tier 0 for public
traffic and tier 1 for reserved traffic. With one waiting request, it is tier
2 for the reserved caller while public traffic can still use it.

Waiting-only configurations also use this order and carry the caller's queue
policy into shared capacity. Deploy router support and update the control
plane's router-config dependency before publishing the new field. Thresholds
reload with configuration without a backend restart.
