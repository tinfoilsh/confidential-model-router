# Observe proposed tier routing

`tier_shadow` collects hypothetical tier decisions while existing routing,
overload limits, priority exceptions, and cache routing remain authoritative.
It defaults off. Keep live `overload` settings unchanged during observation;
candidate thresholds belong only in `tier_shadow`.

```yaml
models:
  gpt-oss-120b:
    repo: example/model
    enclaves: [reserved.example, shared.example]
    overload: {max_requests_waiting: 12}
    tier_shadow: {max_requests_running: 6, max_requests_waiting: 12}
    reservations:
      - org_ids: [example-org]
        enclaves: [reserved.example]
        tier_shadow: {max_requests_running: 3, max_requests_waiting: 1}
```

An omitted reservation block inherits the model's shadow settings. A present
block replaces them. If an organization has multiple reservation entries,
the last recognized entry supplies its shadow policy. Removing all applicable
blocks stops shadow collection. The configuration publisher must preserve
these fields; update its router-config dependency before enabling them.

The candidate order is reserved tier 0, shared tier 0, reserved tier 1, shared
tier 1, unknown load, then overload rejection. Tier 0 has no queue and running
below the candidate limit; tier 1 has not tripped the candidate queue guard.
Queue recovery uses `clear_requests_waiting`, defaulting to half the trip mark.
Callers without a reservation use shared hosts only. Priority and internal
dispatches retain their serve-through-overload semantics in the comparison.

`router_tier_shadow_decisions_total` compares `actual` and `proposed` outcomes
by model and caller pool. Outcomes are `reserved`, `shared`, `reject`,
`unavailable`, or `health_exception`. `comparison` distinguishes a different
pool, different replica, different admission result, unknown metrics, health
exceptions, or agreement. All opted-in selection attempts are counted,
including live rejections and unavailable backends; earlier admission errors
remain in the existing request metrics. No request bodies or organization IDs
are recorded as labels.

Recovery probes are never claimed by shadow evaluation. Probe-eligible and
degraded cases are marked as health exceptions. Random choices count as
agreement when the live replica belongs to the candidate's eligible set.
Cache affinity is read from the existing decision, without recording a
hypothetical cache hit or sending a duplicate inference request.

Shadow adds one separate metrics scrape per second per applicable backend,
per router, at a nominal cadence. It cannot update live overload state.
Scrapes reject redirects and responses exceeding 1 MiB after decompression.
Monitor `router_tier_shadow_scrapes_total`,
`router_tier_shadow_backend_requests`,
`router_tier_shadow_sample_timestamp_seconds`, and
`router_tier_shadow_evaluation_seconds` for collection quality and overhead.
Missing counts affect only policies that require them; samples older than
15 seconds are unknown.

Deploy the same release to all routers with shadow disabled, then enable it
and compare existing latency and error metrics before and after. Shadow
decisions describe today's observed load; they do not predict the changed
queues, cache reuse, or latency after routing is enforced. Stream output-event
gaps also remain distinct from per-token ITL.
