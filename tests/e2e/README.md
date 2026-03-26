# E2E Tests

Tests run against a live router instance using real model calls.

## Quick start

```bash
# 1. Start the router locally (from repo root)
go run . -v

# 2. Run all tests
cd tests/e2e
go test ./... -v -timeout 30m 2>&1 | grep -E "^(--- PASS|--- FAIL|FAIL|ok)" | sed 's/--- PASS/\x1b[32m--- PASS\x1b[0m/g; s/--- FAIL/\x1b[31m--- FAIL\x1b[0m/g; s/^FAIL/\x1b[31mFAIL\x1b[0m/g; s/^ok/\x1b[32mok\x1b[0m/g'
```



Configuration is loaded from `tests/e2e/.env` (copy `.env.example` or edit directly). The defaults point to `http://localhost:8089` with model `gpt-oss-120b`.

## Feature flags

Some tests are skipped unless the corresponding feature is available:

| Variable | Tests enabled |
|---|---|
| `E2E_CI_AVAILABLE=true` | Code interpreter (`chat_ci_test.go`, `responses_ci_test.go`) |
| `E2E_WEBSEARCH_AVAILABLE=true` | Web search (`websearch_test.go`) |
| `E2E_RESPONSES_API_AVAILABLE=true` | `/v1/responses` CI tests |

## Run a subset

```bash
# Regression tests only (no feature flags needed)
go test -run TestRegression ./...

# Single test
go test -run TestChatCI_NonStreaming_BasicExecution ./...
```

## Attested mode

To run against a live enclave with full TLS attestation, set:

```
E2E_TINFOIL_ENCLAVE=inference.tinfoil.sh
E2E_TINFOIL_REPO=tinfoilsh/confidential-model-router
E2E_API_KEY=<your key>
```
