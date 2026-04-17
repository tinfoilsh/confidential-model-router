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

	enclave := model.NextEnclave()
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
	// LOCAL_WEBSEARCH_MCP_ENDPOINT bypasses attested TLS pinning and connects
	// to an arbitrary URL (typically http://127.0.0.1). It is only honored when
	// debug mode is explicitly enabled via the --debug flag or DEBUG env var,
	// so a misconfigured production deployment cannot silently downgrade the
	// trust boundary for the websearch MCP server.
	if em.debug && modelName == "websearch" {
		if endpoint := os.Getenv("LOCAL_WEBSEARCH_MCP_ENDPOINT"); endpoint != "" {
			return endpoint, &http.Client{Timeout: 10 * time.Minute}, nil
		}
	}

	baseURL, client, err := em.boundHTTPClientForModel(modelName)
	if err != nil {
		return "", nil, err
	}
	return baseURL + "/mcp", client, nil
}
