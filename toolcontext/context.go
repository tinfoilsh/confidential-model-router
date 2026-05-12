package toolcontext

import "fmt"

const (
	HeaderRequestID      = "X-Tinfoil-Tool-Request-Id"
	HeaderModel          = "X-Tinfoil-Tool-Model"
	HeaderRoute          = "X-Tinfoil-Tool-Route"
	HeaderStreaming      = "X-Tinfoil-Tool-Streaming"
	HeaderPrincipalType  = "X-Tinfoil-Tool-Principal-Type"
	HeaderPrincipalID    = "X-Tinfoil-Tool-Principal-Id"
	HeaderOrgID          = "X-Tinfoil-Tool-Org-Id"
	HeaderPIICheck       = "X-Tinfoil-Tool-PII-Check"
	HeaderInjectionCheck = "X-Tinfoil-Tool-Injection-Check"

	// Code-execution per-request secrets.
	HeaderCodeExecutionAccessToken        = "X-Code-Execution-Access-Token"
	HeaderCodeExecutionEncryptionKey      = "X-Code-Execution-Encryption-Key"
	HeaderCodeExecutionContainerAuthToken = "X-Code-Execution-Container-Auth-Token"
)

// TinfoilCtx is the router-internal carrier for per-request
// secrets inside the `tinfoil_ctx` body envelope.
// It is extracted once at the router edge (see ExtractTinfoilCtx)
type TinfoilCtx struct {
	AccessToken        string
	EncryptionKey      string
	ContainerAuthToken string
}

// ExtractTinfoilCtx splits an OpenAI-compatible request body into
// its `tinfoil_ctx` envelope (if present) and the underlying
// payload. Three shapes are accepted:
//
//  1. Plain OpenAI body — no `tinfoil_ctx` key. Returns (nil, body,
//     false, nil); caller continues with the body untouched.
//  2. Wrapped body — `{ "tinfoil_ctx": {...}, "payload": {...} }`.
//     Returns (ctx, payload, true, nil); caller should replace its
//     working body with payload and treat it as modified (so the
//     outer re-marshal strips the envelope before forwarding).
//  3. Malformed — `tinfoil_ctx` present but not an object, or
//     `payload` missing/not an object. Returns a non-nil error.
//
// Missing fields inside `tinfoil_ctx` are tolerated (left as empty strings)
func ExtractTinfoilCtx(body map[string]any) (*TinfoilCtx, map[string]any, bool, error) {
	raw, ok := body["tinfoil_ctx"]
	if !ok {
		return nil, body, false, nil
	}

	ctxMap, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, false, fmt.Errorf("tinfoil_ctx must be an object")
	}

	payloadRaw, ok := body["payload"]
	if !ok {
		return nil, nil, false, fmt.Errorf("tinfoil_ctx envelope requires a payload field")
	}
	payload, ok := payloadRaw.(map[string]any)
	if !ok {
		return nil, nil, false, fmt.Errorf("payload must be an object")
	}

	ctx := &TinfoilCtx{
		AccessToken:        stringField(ctxMap, "accessToken"),
		EncryptionKey:      stringField(ctxMap, "encryptionKey"),
		ContainerAuthToken: stringField(ctxMap, "containerAuthToken"),
	}
	return ctx, payload, true, nil
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
