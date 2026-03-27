package manager

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

// UpstreamInvoker issues direct enclave requests without routing back through the
// public HTTP entrypoint.
type UpstreamInvoker struct {
	enclave          *Enclave
	modelName        string
	billingCollector *billing.Collector
	client           *http.Client
}

// NewUpstreamInvoker returns a direct enclave invoker for the provided model.
func (em *EnclaveManager) NewUpstreamInvoker(modelName string) (*UpstreamInvoker, error) {
	model, found := em.GetModel(modelName)
	if !found {
		return nil, ErrModelNotFound(modelName)
	}

	enclave := model.NextEnclave()
	if enclave == nil {
		return nil, ErrNoEnclaveAvailable(modelName)
	}

	return &UpstreamInvoker{
		enclave:          enclave,
		modelName:        modelName,
		billingCollector: em.billingCollector,
		client: &http.Client{
			Transport: &tinfoilClient.TLSBoundRoundTripper{
				ExpectedPublicKey: enclave.tlsKeyFP,
			},
		},
	}, nil
}

// Enclave returns the selected enclave for this invoker.
func (i *UpstreamInvoker) Enclave() *Enclave {
	return i.enclave
}

// Do issues a direct request to the selected enclave using the attested
// transport. The request path and headers are preserved, only the target host is
// rewritten.
func (i *UpstreamInvoker) Do(ctx context.Context, method, requestPath string, header http.Header, body []byte) (*http.Response, error) {
	reqURL := url.URL{
		Scheme: "https",
		Host:   i.enclave.host,
		Path:   requestPath,
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = cloneHeader(header)
	req.ContentLength = int64(len(body))

	return i.client.Do(req)
}

// RecordUsage emits a billing event for a successful direct invocation.
func (i *UpstreamInvoker) RecordUsage(req *http.Request, requestID string, usage *tokencount.Usage, streaming bool) {
	if req == nil {
		return
	}

	userID, apiKey := extractAuthBillingFields(req)
	if apiKey == "" || i.billingCollector == nil {
		return
	}

	event := billing.Event{
		Timestamp:   time.Now(),
		UserID:      userID,
		APIKey:      apiKey,
		Model:       i.modelName,
		RequestID:   requestID,
		Enclave:     i.enclave.host,
		RequestPath: req.URL.Path,
		Streaming:   streaming,
	}
	if usage != nil && i.modelName != websearchModel {
		event.PromptTokens = usage.PromptTokens
		event.CompletionTokens = usage.CompletionTokens
		event.TotalTokens = usage.TotalTokens
	}
	addBillingEvent(i.billingCollector, event)
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return make(http.Header)
	}
	cloned := make(http.Header, len(header))
	for key, values := range header {
		copied := append([]string(nil), values...)
		cloned[key] = copied
	}
	return cloned
}

type errModelNotFound string

func (e errModelNotFound) Error() string {
	return "model " + string(e) + " not found"
}

func (e errModelNotFound) Is(target error) bool {
	_, ok := target.(errModelNotFound)
	return ok
}

// ErrModelNotFound reports a missing model for direct invocation.
func ErrModelNotFound(modelName string) error {
	return errModelNotFound(modelName)
}

type errNoEnclaveAvailable string

func (e errNoEnclaveAvailable) Error() string {
	return "no enclave available for model " + string(e)
}

func (e errNoEnclaveAvailable) Is(target error) bool {
	_, ok := target.(errNoEnclaveAvailable)
	return ok
}

// ErrNoEnclaveAvailable reports a model with no ready enclaves.
func ErrNoEnclaveAvailable(modelName string) error {
	return errNoEnclaveAvailable(modelName)
}
