package manager

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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

func (em *EnclaveManager) nextEnclaveForModel(modelName string) (*Enclave, error) {
	model, found := em.GetModel(modelName)
	if !found {
		return nil, fmt.Errorf("model %s not found", modelName)
	}

	enclave := model.NextEnclave()
	if enclave == nil {
		return nil, fmt.Errorf("model %s has no available enclave", modelName)
	}

	return enclave, nil
}

func newModelRequest(ctx context.Context, host, path string, body []byte, headers http.Header) (*http.Request, error) {
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
	return req, nil
}

func (em *EnclaveManager) ServeModelRequest(ctx context.Context, w http.ResponseWriter, modelName, path string, body []byte, headers http.Header) error {
	enclave, err := em.nextEnclaveForModel(modelName)
	if err != nil {
		return err
	}

	req, err := newModelRequest(ctx, enclave.host, path, body, headers)
	if err != nil {
		return err
	}

	enclave.ServeHTTP(w, req)
	return nil
}

func (em *EnclaveManager) DoModelRequest(ctx context.Context, modelName, path string, body []byte, headers http.Header) (*http.Response, error) {
	recorder := newResponseCapture()
	if err := em.ServeModelRequest(ctx, recorder, modelName, path, body, headers); err != nil {
		return nil, err
	}

	statusCode := recorder.statusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	return &http.Response{
		StatusCode: statusCode,
		Header:     recorder.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(recorder.body.Bytes())),
	}, nil
}

type responseCapture struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
}

func newResponseCapture() *responseCapture {
	return &responseCapture{header: make(http.Header)}
}

func (r *responseCapture) Header() http.Header {
	return r.header
}

func (r *responseCapture) Write(data []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(data)
}

func (r *responseCapture) WriteHeader(statusCode int) {
	if r.statusCode == 0 {
		r.statusCode = statusCode
	}
}

func (r *responseCapture) Flush() {
}

func (em *EnclaveManager) MCPServerEndpoint(modelName string) (string, *http.Client, error) {
	baseURL, client, err := em.boundHTTPClientForModel(modelName)
	if err != nil {
		return "", nil, err
	}
	return baseURL + "/mcp", client, nil
}
