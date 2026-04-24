# Local Testing

This is the router-centric runbook for testing router-owned tool loops
end-to-end. The router talks to real model enclaves for generation but sends
MCP tool traffic to local servers.

One tool profile is currently supported:

- **Web Search** — `confidential-websearch` MCP server

## 1. Start the local MCP server

### Web Search (port 8091)

For standalone bring-up and direct MCP probes, see
`../websearch/local_testing.md`.

#### Fixture mode

```bash
cd ../websearch
LOCAL_TEST_MODE=1 \
LISTEN_ADDR=127.0.0.1:8091 \
go run .
```

`LOCAL_TEST_MODE=1` replaces Exa and Cloudflare with deterministic fixtures.

#### Real-provider mode

```bash
cd ../websearch
set -a && . ./.env && set +a
LISTEN_ADDR=127.0.0.1:8091 \
go run .
```

## 2. Create a small router config

Create a temporary config with the models you want to exercise:

```bash
cat > /tmp/model-router-local.yml <<'EOF'
models:
  gemma4-31b:
    repo: tinfoilsh/confidential-gemma4-31b
    enclaves:
      - gemma4-31b.inf9.tinfoil.sh
  gpt-oss-120b:
    repo: tinfoilsh/confidential-gpt-oss-120b
    enclaves:
      - gpt-oss-120b-0.inf9.tinfoil.sh
  kimi-k2-6:
    repo: tinfoilsh/confidential-kimi-k2-6
    enclaves:
      - kimi-k2-6.tinfoil.containers.tinfoil.dev

EOF
```

Compute its checksum:

```bash
shasum -a 256 /tmp/model-router-local.yml
```

## 3. Start the local router

From the `confidential-model-router` repo. Include `LOCAL_MCP_ENDPOINT_*`
variables for whichever MCP servers you started in step 1:

```bash
DEBUG=1 \
LOCAL_MCP_ENDPOINT_WEBSEARCH=http://127.0.0.1:8091/mcp \
PORT=8090 \
DOMAIN=localhost \
INIT_CONFIG_URL="/tmp/model-router-local.yml@sha256:<sha-from-step-2>" \
UPDATE_CONFIG_URL=/tmp/model-router-local.yml \
USAGE_REPORTER_SECRET=test-secret \
go run -tags toolruntime_debug .
```

`LOCAL_MCP_ENDPOINT_<MODEL>` makes router-owned tool calls for the named MCP
model hit a local MCP server instead of the attested deployment. The model
name is upper-cased with non-alphanumeric characters replaced by underscores,
so `websearch` becomes `LOCAL_MCP_ENDPOINT_WEBSEARCH`. These overrides are
only honored when debug mode is enabled (via `DEBUG=1` or the `--debug` flag),
which prevents a misconfigured production deployment from silently downgrading
to a non-attested HTTP endpoint.

### Toolruntime tracing

The per-request `toolruntime:<tid>` tracing emitted by `debugLogf` is gated
purely at compile time by the `toolruntime_debug` build tag. Without the tag,
`debugEnabled` is a compile-time `false` constant and every call site is
eliminated by the Go compiler, so production TEE images carry zero debug code:

- `go run .` / `go build .` (default, and `go build -tags prod .`): tracing is compiled out.
- `go run -tags toolruntime_debug .` / `go build -tags toolruntime_debug .`: tracing is compiled in and always on.

If you want the `toolruntime:<tid> ...` lines in this runbook, build with the
tag as shown above. Otherwise you will still see `DEBUG=1` router logs but
none of the per-iteration tool-loop trace.

---

## Web Search smoke tests

If the websearch server is running in fixture mode, these prompts should
return the quoted answers.

### Chat Completions

```bash
curl -sS -X POST http://127.0.0.1:8090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TINFOIL_API_KEY" \
  -d '{
    "model": "gemma4-31b",
    "web_search_options": {},
    "messages": [
      {
        "role": "user",
        "content": "Search the web once and answer this question in one sentence: According to the Local Cat Almanac 2026, what does Nimbus do after breakfast?"
      }
    ],
    "max_tokens": 120
  }'
```

Expected answer:

```text
According to the Local Cat Almanac 2026, Nimbus naps for exactly 17 minutes after breakfast before inspecting the window.
```

### Responses

```bash
curl -sS -X POST http://127.0.0.1:8090/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TINFOIL_API_KEY" \
  -d '{
    "model": "gpt-oss-120b",
    "input": "Search the web once and answer this question in one sentence: According to the Neighborhood Cat Gazette, which cushions do the cats in the sunroom prefer?",
    "stream": false,
    "temperature": 0,
    "max_output_tokens": 120,
    "tools": [{"type": "web_search"}]
  }'
```

Expected answer:

```text
According to the Neighborhood Cat Gazette, the cats in the sunroom prefer saffron cushions because they stay warm in the afternoon light.
```

### Eval harness

```bash
cd ../websearch
TINFOIL_API_KEY=... python3 evals/run_websearch_eval.py --base http://127.0.0.1:8090

# If the local websearch server is running with LOCAL_TEST_MODE=1
TINFOIL_API_KEY=... python3 evals/run_websearch_eval.py \
  --base http://127.0.0.1:8090 \
  --local-fixtures \
  --mcp-base http://127.0.0.1:8091
```

For the matrix layout and result analyzer details, see
`../websearch/evals/WEBSEARCH_EVAL.md`.

---

## Verified model matrix

These combinations were validated locally:

- `gemma4-31b`
  - `/v1/chat/completions`
  - non-streaming and streaming
- `gpt-oss-120b`
  - `/v1/chat/completions`
  - non-streaming and streaming
  - `/v1/responses`
  - non-streaming and streaming

For the `gpt-oss-120b` Responses API path, the most stable local settings were:

- `"temperature": 0`
- `"max_output_tokens": 120` for non-streaming
- `"max_output_tokens": 400` for streaming

`qwen3-vl-30b` was not added to the recommended local matrix because
attestation fetches for its current enclave were failing during this test run.

## Cleanup

If you launched processes in interactive shells, `Ctrl+C` in each is enough.

If they are still running in the background, kill by port:

```bash
lsof -ti tcp:8091 | xargs kill   # websearch MCP server
lsof -ti tcp:8090 | xargs kill   # model-router
```
