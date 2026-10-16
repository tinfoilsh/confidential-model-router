# Pool routing and overload

The model's `overload` policy applies to public callers. A reservation's
`overload` block replaces it for that reservation's callers, including on
shared capacity. Omission inherits the model policy; `{}` disables it.
Multiple reservations for one organization must agree on the policy.

| Tier | Meaning under the caller's policy |
| --- | --- |
| 0 | No waiting requests; running below `max_requests_running`, if set |
| 1 | Not tier 0, but the queue guard has not tripped |
| 2 | Queue reached `max_requests_waiting` and has not recovered |

Reserved callers prefer **reserved 0 → shared 0 → reserved 1 → shared 1**.
Cache affinity wins within the selected pool and tier, subject to
`cache_route.max_inflight_delta`. Public callers use shared hosts only.
Without reservations, all hosts form one pool.

`max_requests_running` is a soft preference, never a rejection threshold.
For example, public running/waiting limits of **8/14** and reserved limits
of **4/14** make a shared host with four running requests and no queue tier 0
for public callers and tier 1 for reserved callers. Neither policy rejects it.
Nonpositive limits disable their respective thresholds.

Queue overload clears at `clear_requests_waiting`, defaulting to half the
waiting limit rounded down. If every usable candidate is tier 2, ordinary
requests receive **HTTP 503** with `Retry-After` (default: one minute).
Configured priority requests and internal dispatches serve through overload.

Metrics polling has a nominal one-second interval; slow scrapes reduce it.
Selection reserves local in-flight load atomically so simultaneous arrivals
respect the affinity cap. Routers track this independently; sampled running
limits are not global concurrency caps.
Missing/incomplete samples or samples older than 15 seconds are unknown:
known tiers 0 and 1 win first, then unknown load fails open. Expired
attestations are excluded; circuit-breaker recovery probes and the
all-breakers-open fallback retain their load-guard bypass.

`router_reserved_pool_serves_total{pool="spilled"}` counts spillover.
Backend overload gauges describe the host's own policy, not every caller's tier.
Deploy router support and update the control plane's router-config dependency
before publishing `max_requests_running`. Policies reload without a restart.
