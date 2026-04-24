package manager

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

func (em *EnclaveManager) boundHTTPClientForModel(modelName string) (string, *http.Client, error) {
	model, found := em.GetModel(modelName)
	if !found {
		return "", nil, fmt.Errorf("model %s not found", modelName)
	}

	enclave := model.NextEnclave(nil)
	if enclave == nil {
		return "", nil, fmt.Errorf("model %s has no available enclave", modelName)
	}

	client := &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &slowHeaderTripper{
			base: &tinfoilClient.TLSBoundRoundTripper{
				ExpectedPublicKey: enclave.tlsKeyFP,
			},
			timeout: responseHeaderTimeout,
			onSlow:  func() {},
		},
	}

	return "https://" + enclave.host, client, nil
}

// DoModelRequest performs an attested POST against the next available enclave
// for the given model. It bypasses the public reverse proxy (and therefore its
// billing/usage hooks) so that internal callers can be responsible for their
// own billing accounting without needing a trust-the-caller HTTP header.
func (em *EnclaveManager) DoModelRequest(ctx context.Context, modelName, path string, body []byte, headers http.Header) (*http.Response, error) {
	baseURL, client, err := em.boundHTTPClientForModel(modelName)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
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
			return endpoint, &http.Client{Timeout: 10 * time.Minute}, nil
		}
	}

	baseURL, client, err := em.boundHTTPClientForModel(modelName)
	if err != nil {
		return "", nil, err
	}
	return baseURL + "/mcp", client, nil
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
