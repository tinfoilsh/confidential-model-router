# Runtime overload policies

`models.<model>.overload` supplies the default queue policy for the model's backends. Each entry in `reservations` can supply an `overload` block for its reserved backends:

```yaml
models:
  gpt-oss-120b:
    repo: tinfoilsh/confidential-gpt-oss-120b
    enclaves: [shared.example, reserved.example]
    overload:
      max_requests_waiting: 14
      retry_after_minutes: 1
    reservations:
      - org_ids: [org_reserved]
        enclaves: [reserved.example]
        overload:
          max_requests_waiting: 1
          retry_after_minutes: 1
```

The reserved backend trips at one waiting request and clears when its queue is empty. The shared backend trips at 14 and clears at seven. These policies follow the backend: a reserved caller spilling into the shared pool encounters the shared backend's policy. Existing configured-priority exemptions still apply.

- An omitted or `null` reservation `overload` inherits the entire model block.
- A supplied reservation block replaces the entire model block; individual fields do not inherit. An explicit `max_requests_waiting: 0` or empty block disables the guard for those backends.
- The existing fields and defaults apply at either level: `clear_requests_waiting` defaults to half the trip threshold when omitted, zero, negative, or at/above the trip threshold; a nonpositive `retry_after_minutes` defaults to one minute.
- A reservation can enable its guard even when the model has no overload policy. Multiple organizations can share one reservation. An organization can have several reservations, each with its own backend policy; a backend must belong to only one reservation.

Queue policies are refreshed with the runtime configuration and applied when a backend is verified or replaced. Removing an override, or moving a backend out of a reservation, restores the model policy. The guard uses sampled queue depth; it is not a hard cap on admitted requests or vLLM's running sequences.

Upgrade every router to a release supporting reservation overloads and update the controlplane validator before publishing these fields. Older routers ignore the nested block and continue applying the model policy. Cache-routing settings and vLLM startup flags remain model-level and deployment settings, respectively.
