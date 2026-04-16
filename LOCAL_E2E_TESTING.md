# Local End-to-End Testing

This flow runs:

- a local `confidential-websearch` MCP server with deterministic test fixtures
- a local `confidential-model-router`
- a real model request through the router with `web_search_options`

The router still talks to the real model enclave for generation, but it sends MCP tool traffic to your local websearch server.

## 1. Start the local websearch MCP server

From the `confidential-websearch` repo:

```bash
LOCAL_TEST_MODE=1 \
LISTEN_ADDR=127.0.0.1:8091 \
go run .
```

`LOCAL_TEST_MODE=1` replaces external search and fetch dependencies with local fixtures, so you do not need Exa or Cloudflare credentials for this flow.

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
EOF
```

Compute its checksum:

```bash
shasum -a 256 /tmp/model-router-local.yml
```

## 3. Start the local router

From the `confidential-model-router` repo:

```bash
LOCAL_WEBSEARCH_MCP_ENDPOINT=http://127.0.0.1:8091/mcp \
PORT=8090 \
DOMAIN=localhost \
INIT_CONFIG_URL="/tmp/model-router-local.yml@sha256:<sha-from-step-2>" \
UPDATE_CONFIG_URL=/tmp/model-router-local.yml \
go run .
```

`LOCAL_WEBSEARCH_MCP_ENDPOINT` makes router-owned web search tool calls hit the local MCP server instead of the attested `websearch` deployment.

## 4. Verified model matrix

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

`qwen3-vl-30b` was not added to the recommended local matrix because attestation fetches for its current enclave were failing during this test run.

## 5. Run local end-to-end queries

Use your normal API key against the local router:

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

Another useful probe:

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
        "content": "Search the web once and answer this question in one sentence: According to the Neighborhood Cat Gazette, which cushions do the cats in the sunroom prefer?"
      }
    ],
    "max_tokens": 120
  }'
```

Expected answer:

```text
According to the Neighborhood Cat Gazette, the cats in the sunroom prefer saffron cushions because they stay warm in the afternoon light.
```

### Responses API probes

Non-streaming Responses:

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

Streaming Responses:

```bash
curl -sS -N -X POST http://127.0.0.1:8090/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TINFOIL_API_KEY" \
  -d '{
    "model": "gpt-oss-120b",
    "input": "Search the web once and answer this question in one sentence: According to the Local Cat Almanac 2026, what does Nimbus do after breakfast?",
    "stream": true,
    "temperature": 0,
    "max_output_tokens": 400,
    "tools": [{"type": "web_search"}]
  }'
```

### Chat Completions probes

Non-streaming Chat Completions:

```bash
curl -sS -X POST http://127.0.0.1:8090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TINFOIL_API_KEY" \
  -d '{
    "model": "gpt-oss-120b",
    "web_search_options": {},
    "stream": false,
    "messages": [
      {
        "role": "user",
        "content": "Search the web once and answer this question in one sentence: According to the Neighborhood Cat Gazette, which cushions do the cats in the sunroom prefer?"
      }
    ],
    "max_tokens": 120
  }'
```

Streaming Chat Completions:

```bash
curl -sS -N -X POST http://127.0.0.1:8090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TINFOIL_API_KEY" \
  -d '{
    "model": "gemma4-31b",
    "web_search_options": {},
    "stream": true,
    "messages": [
      {
        "role": "user",
        "content": "Search the web once and answer this question in one sentence: According to the Local Cat Almanac 2026, what does Nimbus do after breakfast?"
      }
    ],
    "max_tokens": 120
  }'
```

The old matrix driver in `websearch/evals/run_websearch_eval.sh` is still a useful reference for request shapes, but it is not yet wired for this local router+MCP flow or authenticated local runs.

## 6. What local test mode does

When `LOCAL_TEST_MODE=1` is enabled in websearch:

- `search` returns deterministic local fixtures
- `fetch` returns deterministic page bodies for those fixtures
- no Exa dependency is required
- no Cloudflare dependency is required

The current fixture URLs are:

- `https://local.test/cats/almanac`
- `https://local.test/cats/gazette`

## 7. Cleanup

If you launched both processes in interactive shells, `Ctrl+C` in each is enough.

If they are still running in the background, kill by port rather than by path
so cleanup works regardless of where you launched the binaries from:

```bash
lsof -ti tcp:8091 | xargs kill   # websearch MCP server
lsof -ti tcp:8090 | xargs kill   # model-router
```
