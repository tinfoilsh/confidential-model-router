# Backend trust

Each model requires `repo`, `config_ref`, or both:

```yaml
models:
  gpt-oss-120b:
    repo: tinfoilsh/confidential-gpt-oss-120b
    config_ref: tinfoil/confidential-gpt-oss-120b
    enclaves: [classic.example, registry.example]
```

`repo` authorizes GitHub repository-backed attestations. `config_ref` authorizes
registry-backed attestations for exactly `namespace/project`; it must not include
a leading slash, revision, digest, or URL. Any revision of that project can pass
if its config, runtime, platform, hardware binding, and freshness pass the SDK's
verification. A matching name in the other attestation flow is not sufficient.

Setting both allows classic and IGVM backends in the same pool during migration.
Trust anchors come from the router's bundled config or its hash-pinned initial
config. Adding, removing, or changing either anchor requires a router release;
live updates may change the model's enclave list and routing settings only when
both anchors match. Upgrade the control-plane config validator before publishing
`config_ref` in the live config.

The router's own `tinfoil-config.yml` and firmware release flow are independent
of backend verification. Backend status includes the authenticated `config_name`
and `config_digest` for registry deployments. Their `code_tag` identifies the
runtime release; for classic backends it identifies the repository release.
