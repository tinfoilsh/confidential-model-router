package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"

	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"

	"github.com/tinfoilsh/confidential-model-router/cacheroute"
)

func (em *EnclaveManager) boundHTTPClientForModel(modelName string) (string, *http.Client, error) {
	enclave, client, err := em.boundHTTPClientPreferring(modelName, nil)
	if err != nil {
		return "", nil, err
	}
	return enclave.host, client, nil
}

// boundHTTPClientPreferring picks the serving enclave for an internal
// dispatch — honoring a cache-aware host preference with overload spill (see
// Model.SelectForDispatch) — and builds its attested client.
func (em *EnclaveManager) boundHTTPClientPreferring(modelName string, order []string) (*Enclave, *http.Client, error) {
	model, found := em.GetModel(modelName)
	if !found {
		return nil, nil, fmt.Errorf("model %s not found", modelName)
	}

	enclave := model.SelectForDispatch(order)
	if enclave == nil {
		return nil, nil, fmt.Errorf("model %s has no available enclave", modelName)
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

	return enclave, client, nil
}

// DoModelRequest performs an attested POST against the next available enclave
// for the given model. It bypasses the public reverse proxy (and therefore its
// billing/usage hooks) so that internal callers can be responsible for their
// own billing accounting without needing a trust-the-caller HTTP header.
func (em *EnclaveManager) DoModelRequest(ctx context.Context, modelName, path string, body []byte, headers http.Header) (*http.Response, error) {
	enclave, client, err := em.boundHTTPClientPreferring(modelName, nil)
	if err != nil {
		return nil, err
	}
	return postToEnclave(ctx, client, enclave, path, body, headers)
}

// DoModelRequestJSON marshals and dispatches a parsed request body the way
// DoModelRequest does, threading it through cache-aware routing. The tool
// loop calls this for every model iteration — replaying a growing shared
// prefix, the strongest case for cache-aware routing — so those requests are
// measured (and, on enforced pools, pinned) like plain proxied ones. The key
// stays stable across iterations while the request shape does (the forced
// final turn drops tool definitions and re-keys; see toolruntime).
func (em *EnclaveManager) DoModelRequestJSON(ctx context.Context, modelName, path string, body map[string]any, headers http.Header) (*http.Response, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, settings, pool, ok := em.cacheRouteRequest(modelName, path, body)
	var decision *cacheroute.Decision
	if ok && settings.Mode == cacheroute.ModeEnforced {
		decision = em.cacheRouteShadow.Decide(modelName, req, pool, settings)
	}
	var order []string
	if decision != nil {
		order = decision.Order
	}

	enclave, client, err := em.boundHTTPClientPreferring(modelName, order)
	if err != nil {
		return nil, err
	}
	resp, err := postToEnclave(ctx, client, enclave, path, bodyBytes, headers)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		if decision != nil {
			em.cacheRouteShadow.ObserveLanding(modelName, req, decision, enclave.host, settings)
		} else if ok {
			em.cacheRouteShadow.Observe(modelName, req, pool, enclave.host, settings)
		}
	}
	return resp, nil
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

// postToEnclave builds and sends an attested POST to one enclave, counting
// it in the enclave's in-flight gauge for the lifetime of the response body
// so internal dispatches are visible to least-loaded selection like proxied
// requests are.
func postToEnclave(ctx context.Context, client *http.Client, enclave *Enclave, path string, body []byte, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+enclave.host+path, bytes.NewReader(body))
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

	enclave.inflight.Add(1)
	resp, err := client.Do(req)
	if err != nil {
		enclave.inflight.Add(-1)
		reason := classifyProxyError(err)
		if reason == "canceled" {
			ClientCancellationsTotal.WithLabelValues(enclave.modelName, enclave.host).Inc()
			if enclave.cb != nil && enclave.cb.AbortProbe() {
				publishBreakerState(enclave.modelName, enclave.host, enclave.cb)
			}
		} else {
			ProxyFailureTotal.WithLabelValues(enclave.modelName, enclave.host, reason).Inc()
			if enclave.cb != nil {
				enclave.cb.RecordFailure()
				publishBreakerState(enclave.modelName, enclave.host, enclave.cb)
			}
		}
		return nil, err
	}
	if resp.StatusCode >= 500 {
		ProxyFailureTotal.WithLabelValues(enclave.modelName, enclave.host, httpFailureReason(resp.StatusCode)).Inc()
		if enclave.cb != nil {
			enclave.cb.RecordFailure()
		}
	} else {
		ProxySuccessTotal.WithLabelValues(enclave.modelName, enclave.host).Inc()
		if enclave.cb != nil {
			enclave.cb.RecordSuccess()
		}
	}
	if enclave.cb != nil {
		publishBreakerState(enclave.modelName, enclave.host, enclave.cb)
	}
	resp.Body = &inflightBody{ReadCloser: resp.Body, enclave: enclave}
	return resp, nil
}

// inflightBody decrements the enclave's in-flight gauge when the response
// body is closed — client.Do returns at response headers, so for streaming
// responses the request is still in flight until the consumer finishes.
type inflightBody struct {
	io.ReadCloser
	enclave *Enclave
	closed  atomic.Bool
}

// Close is idempotent, like net/http's own response bodies: the first call
// decrements the gauge and closes the underlying body, later calls do
// nothing and return nil.
func (b *inflightBody) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.enclave.inflight.Add(-1)
	return b.ReadCloser.Close()
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
