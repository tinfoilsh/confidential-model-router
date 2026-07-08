package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"

	"github.com/tinfoilsh/confidential-model-router/cacheroute"
)

func (em *EnclaveManager) boundHTTPClientForModel(modelName string) (string, *http.Client, error) {
	return em.boundHTTPClientPreferring(modelName, nil)
}

// boundHTTPClientPreferring is boundHTTPClientForModel with a cache-aware
// host preference (see Model.NextEnclavePreferring).
func (em *EnclaveManager) boundHTTPClientPreferring(modelName string, order []string) (string, *http.Client, error) {
	model, found := em.GetModel(modelName)
	if !found {
		return "", nil, fmt.Errorf("model %s not found", modelName)
	}

	enclave := model.NextEnclavePreferring(order, nil)
	if enclave == nil {
		return "", nil, fmt.Errorf("model %s has no available enclave", modelName)
	}

	// No client-level Timeout: it would cover the entire exchange including
	// reading the streamed response body, hard-killing long-running reasoning
	// streams mid-flight. Requests are bounded by the caller's context (the
	// incoming client request), matching the passive behavior of the public
	// reverse proxy path.
	client := &http.Client{
		Transport: &slowHeaderTripper{
			base: &tinfoilClient.TLSBoundRoundTripper{
				ExpectedPublicKey: enclave.tlsKeyFP,
			},
			timeout: responseHeaderTimeout,
			onSlow:  func() {},
		},
	}

	return enclave.host, client, nil
}

// DoModelRequest performs an attested POST against the next available enclave
// for the given model. It bypasses the public reverse proxy (and therefore its
// billing/usage hooks) so that internal callers can be responsible for their
// own billing accounting without needing a trust-the-caller HTTP header.
func (em *EnclaveManager) DoModelRequest(ctx context.Context, modelName, path string, body []byte, headers http.Header) (*http.Response, error) {
	host, client, err := em.boundHTTPClientForModel(modelName)
	if err != nil {
		return nil, err
	}
	return postToEnclave(ctx, client, host, path, body, headers)
}

// DoModelRequestJSON marshals and dispatches a parsed request body the way
// DoModelRequest does, threading it through cache-aware routing. The tool
// loop calls this for every model iteration — replaying a growing shared
// prefix, the strongest case for cache-aware routing — so those requests are
// measured (and, on enforced pools, pinned) like plain proxied ones: the
// key is stable across iterations, so the whole loop homes to one replica.
func (em *EnclaveManager) DoModelRequestJSON(ctx context.Context, modelName, path string, body map[string]any, headers http.Header) (*http.Response, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, settings, pool, ok := em.cacheRouteRequest(modelName, path, body)
	var order []string
	if ok && settings.Mode == cacheroute.ModeEnforced {
		order = em.cacheRouteShadow.PreferenceOrder(modelName, req, pool, settings)
	}

	host, client, err := em.boundHTTPClientPreferring(modelName, order)
	if err != nil {
		return nil, err
	}
	// Observed at dispatch, like the public proxy path, so the picked
	// replica counts as warm from prefill start.
	if ok {
		em.cacheRouteShadow.Observe(modelName, req, pool, host, settings)
	}
	return postToEnclave(ctx, client, host, path, bodyBytes, headers)
}

// cacheRouteRequest classifies one internally dispatched request for the
// cache-route pipeline, with the same gating as the public path, returning
// the pool snapshot the decision should be made over. Callers only dispatch
// to cache_salt-capable endpoints, so no endpoint allowlist is re-checked
// here. ok is false when the pipeline is off for the model (or the manager
// has no shadow, as in tests).
func (em *EnclaveManager) cacheRouteRequest(modelName, path string, body map[string]any) (*cacheroute.Request, cacheroute.Settings, cacheroute.Pool, bool) {
	if em.cacheRouteShadow == nil {
		return nil, cacheroute.Settings{}, cacheroute.Pool{}, false
	}
	model, found := em.GetModel(modelName)
	if !found {
		return nil, cacheroute.Settings{}, cacheroute.Pool{}, false
	}
	settings := model.CacheRouteSettings()
	if settings.Mode == cacheroute.ModeOff {
		return nil, cacheroute.Settings{}, cacheroute.Pool{}, false
	}
	salt, _ := body["cache_salt"].(string)
	req := cacheroute.ExtractRequest(body, path, salt, settings)
	return req, settings, model.CacheRoutePool(), true
}

// postToEnclave builds and sends an attested POST to one enclave host.
func postToEnclave(ctx context.Context, client *http.Client, host, path string, body []byte, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

	return client.Do(req)
}

func (em *EnclaveManager) MCPServerEndpoint(modelName string) (string, *http.Client, error) {
	// LOCAL_MCP_ENDPOINT_<MODEL_NAME> bypasses attested TLS pinning
	// and connects to an arbitrary URL (typically http://127.0.0.1).
	// Honored only when --debug / DEBUG is explicitly enabled so a
	// mis-configured production deployment cannot silently downgrade
	// the trust boundary to a non-attested HTTP endpoint. The model
	// name is upper-cased with non-alphanumeric characters replaced
	// by underscores; see localMCPEndpointEnvVar.
	if em.debug {
		if endpoint := os.Getenv(localMCPEndpointEnvVar(modelName)); endpoint != "" {
			return endpoint, &http.Client{}, nil
		}
	}

	host, client, err := em.boundHTTPClientForModel(modelName)
	if err != nil {
		return "", nil, err
	}
	return "https://" + host + "/mcp", client, nil
}

// localMCPEndpointEnvVar returns the model-specific debug-bypass env
// var name for a given MCP model. Example: "code-runner" ->
// "LOCAL_MCP_ENDPOINT_CODE_RUNNER".
func localMCPEndpointEnvVar(modelName string) string {
	var b []byte
	for _, r := range modelName {
		switch {
		case r >= 'a' && r <= 'z':
			b = append(b, byte(r-'a'+'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	return "LOCAL_MCP_ENDPOINT_" + string(b)
}
