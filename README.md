# Confidential Inference Router

Tinfoil's model router is the entry point for confidential inference. It terminates TLS (optionally with [EHBP](https://docs.tinfoil.sh/resources/ehbp)), reads the model name from an OpenAI-compatible request, and forwards the request to a verified secure enclave serving that model.

## How it works

For each request the router:

1. Authenticates the caller and checks rate limits with the control plane
2. Resolves the requested model (or picks one for `model: "auto"`) to a healthy enclave
3. Runs any server-side tools the request asked for (web search, code execution, PII check)
4. Streams the enclave's response back to the client

Client-facing behavior is documented at [docs.tinfoil.sh](https://docs.tinfoil.sh):

- [Models](https://docs.tinfoil.sh/models/overview)
- [Reasoning](https://docs.tinfoil.sh/guides/reasoning)
- [Web search](https://docs.tinfoil.sh/guides/web-search)
- [Tool calling](https://docs.tinfoil.sh/guides/tool-calling)
- [Direct API access](https://docs.tinfoil.sh/sdk/direct-api-access)

## Architecture Overview

- **[main.go](main.go)**: Request handling, model resolution, and forwarding to enclaves
- **[autoroute/](autoroute/)**: Model and reasoning-effort selection for `model: "auto"`
- **[route_context.go](route_context.go)**: Admission and rate-limit decisions from the control plane
- **[toolruntime/](toolruntime/)**: Server-side tool loops (web search, code execution)
- **[safeguards/](safeguards/)**: Asynchronous acceptable-use checks for first-party chat
- **[billing/](billing/)**: Usage reporting

## Reporting Vulnerabilities

Please report security vulnerabilities by either:

- Emailing [security@tinfoil.sh](mailto:security@tinfoil.sh)
- Opening an issue on GitHub on this repository

We aim to respond to (legitimate) security reports within 24 hours.
