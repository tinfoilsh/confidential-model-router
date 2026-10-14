# Pool routing and overload

Set a positive `overload.max_requests_running` to enable tiers. The model's
policy applies to public requests; a reservation's `overload` block replaces
it for that reservation's callers. An omitted block inherits the model policy;
an empty block disables it. A tiered caller keeps its policy when spilling
to shared capacity, so the same shared replica can have different tiers for
different callers. Entries reserving multiple groups of hosts for one
organization must specify the same effective tier policy.

## Tiers and selection

| Tier | Meaning under the caller's policy |
| --- | --- |
| 0 | No waiting requests and running requests below `max_requests_running` |
| 1 | Not tier 0, but the queue guard has not tripped |
| 2 | Queue reached `max_requests_waiting` and has not recovered |

Reserved requests prefer **reserved 0 → shared 0 → reserved 1 → shared 1**.
Cache affinity breaks ties within the selected pool and tier, subject to
`cache_route.max_inflight_delta`. Affinity estimates prefix reuse; it does not
verify current KV-cache contents. Public requests consider shared capacity
only, even if reserved hosts are idle. Without reservations, all hosts form
one pool.

Crossing the running threshold changes preference, never rejects a request
by itself. Tier 2 takes precedence over the running count. A tripped queue
guard recovers at or below `clear_requests_waiting`, defaulting to half the
waiting limit, rounded down. A waiting limit of 1 clears at zero. Recovery
state is tracked separately for each configured pair of queue thresholds.
A nonpositive waiting limit disables hard overload rejection.

If every usable candidate is tier 2, ordinary requests receive **HTTP 503**
with `Retry-After` from the caller's policy (default: one minute). Configured
priority requests and internal dispatches retain their serve-through-overload
behavior, using the same preference order.

## Metrics and health

The router polls vLLM's running/waiting gauges every second. These are sampled
preferences, not strict concurrency caps; concurrent arrivals and other
routers can exceed them. This policy adds no admission delay or batching wait.
Shared hosts are polled when a reserved spillover policy needs their metrics,
even if their own guard is disabled.

Missing/incomplete samples or samples older than 15 seconds are **unknown**,
never tier 0. Known tier 0 and tier 1 capacity wins first. If none exists,
unknown candidates fail open, preferring reserved over shared. Expired
attestations are always excluded. Due circuit-breaker recovery probes bypass
load preferences when their pool is considered and must be dispatched once
claimed. If no closed backend exists, the existing degraded health fallback
also remains available.

The existing `router_reserved_pool_serves_total{pool="spilled"}` counter
counts spillover dispatches, including tier-based spillover. Backend overload
gauges describe each backend's native queue policy, not every caller's tier.

## Example

Illustrative values, not workload recommendations:

```yaml
models:
  gpt-oss-120b:
    repo: example/model
    enclaves: [reserved.example, shared.example]
    overload:
      max_requests_running: 6
      max_requests_waiting: 12
      retry_after_minutes: 1
    reservations:
      - org_ids: [example-org]
        enclaves: [reserved.example]
        overload:
          max_requests_running: 3
          max_requests_waiting: 1
          retry_after_minutes: 1
```

Here, a shared host with four running requests and no queue is tier 0 for a
public caller and tier 1 for the reserved caller. With one waiting request,
it becomes tier 2 for the reserved caller while public traffic can still use it.

Queue-only configurations (no positive running threshold) retain the existing
per-backend overload behavior. Deploy router support and update the control
plane's router-config dependency before publishing the new field. Threshold
changes reload with configuration; they do not require a backend restart.
