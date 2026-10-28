package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/tinfoilsh/tinfoil-go/enclave"
)

const attestationRecoveryTimeout = 10 * time.Second

// rotationTransport retries only handshake rejections, before HTTP headers have
// been sent. HTTP errors and failures while reading a response are not replayed.
type rotationTransport struct {
	base    http.RoundTripper
	refresh func(context.Context) (http.RoundTripper, error)
}

func (t *rotationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }}
	attempt := req.Clone(httptrace.WithClientTrace(req.Context(), trace))
	// Keep replay ownership here rather than enabling the HTTP transport's
	// independent retry policy for requests with idempotency headers.
	attempt.GetBody = nil
	response, err := t.base.RoundTrip(attempt)
	var attestationErr *enclave.AttestationError
	if response != nil || !errors.As(err, &attestationErr) || errors.Is(err, errAttestationExpired) || sent.Load() || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) {
		return response, err
	}
	ctx, cancel := context.WithTimeout(req.Context(), attestationRecoveryTimeout)
	defer cancel()
	transport, refreshErr := t.refresh(ctx)
	if refreshErr != nil {
		return nil, errors.Join(err, refreshErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	retry := req.Clone(req.Context())
	retry.GetBody = nil
	if req.Body != nil && req.Body != http.NoBody {
		retry.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
	}
	return transport.RoundTrip(retry)
}

func (em *EnclaveManager) recoverEnclave(ctx context.Context, modelName, host, failedKey string) (http.RoundTripper, error) {
	model, found := em.GetModel(modelName)
	if !found {
		return nil, fmt.Errorf("model %s is no longer configured", modelName)
	}
	result := em.verifications.DoChan(modelName+"\x00"+host, func() (any, error) {
		model.mu.RLock()
		_, configured := model.configuredHosts[host]
		current := model.Enclaves[host]
		changed := current != nil && current.tlsKeyFP != failedKey
		model.mu.RUnlock()
		if !configured {
			return nil, fmt.Errorf("enclave %s is no longer configured", host)
		}
		if changed {
			return nil, nil
		}
		return nil, em.refreshEnclave(modelName, host, true)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case outcome := <-result:
		if outcome.Err != nil {
			return nil, outcome.Err
		}
	}
	model.mu.RLock()
	defer model.mu.RUnlock()
	current := model.Enclaves[host]
	_, configured := model.configuredHosts[host]
	if !configured || current == nil || current.tlsKeyFP == failedKey || !current.attestationValid(time.Now()) {
		return nil, fmt.Errorf("no freshly verified replacement for enclave %s", host)
	}
	// Use the verified replacement's transport directly so a second rotation
	// cannot recursively replay this request.
	return current.proxy.Transport.(*rotationTransport).base, nil
}
