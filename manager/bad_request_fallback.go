package manager

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

type badRequestFallbackKey struct{}

type badRequestFallback struct {
	model      *Model
	order      []string
	onFallback func(*Enclave)
}

// WithBadRequestFallback permits one alternate inference dispatch before any
// response is sent to the caller. onFallback identifies the accepted backend.
func (m *Model) WithBadRequestFallback(ctx context.Context, order []string, onFallback func(*Enclave)) context.Context {
	return context.WithValue(ctx, badRequestFallbackKey{}, &badRequestFallback{m, order, onFallback})
}

func recordHTTPOutcome(model, host string, cb *circuitBreaker, resp *http.Response) {
	switch {
	case resp.StatusCode >= http.StatusInternalServerError:
		ProxyFailureTotal.WithLabelValues(model, host, httpFailureReason(resp.StatusCode)).Inc()
		if cb != nil {
			cb.RecordFailure()
		}
	case resp.StatusCode >= http.StatusBadRequest:
		ProxyFailureTotal.WithLabelValues(model, host, httpFailureReason(resp.StatusCode)).Inc()
		ProbeClaimFromContext(resp.Request.Context()).Abort()
	default:
		ProxySuccessTotal.WithLabelValues(model, host).Inc()
		if cb != nil {
			cb.RecordSuccess()
		}
	}
	if cb != nil {
		publishBreakerState(model, host, cb)
	}
}

func canRetryBadRequest(resp *http.Response) bool {
	req := resp.Request
	if resp.StatusCode != http.StatusBadRequest || req.Method != http.MethodPost || req.GetBody == nil || req.Context().Err() != nil {
		return false
	}
	switch req.URL.Path {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses":
	default:
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(body)
	if decoder.Decode(&fields) != nil || fields == nil {
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return false
	}
	// Backend-local response and conversation IDs can legitimately exist on
	// only one replica; a different answer there is not evidence of a bad build.
	for _, key := range []string{"previous_response_id", "conversation"} {
		if value, ok := fields[key]; ok && string(value) != "null" {
			return false
		}
	}
	return true
}

func (m *Model) badRequestAlternative(failed *Enclave, req *http.Request, order []string) *Enclave {
	primary, spill := m.ReservationPools(CallerOrgFromContext(req.Context()))
	eligiblePrimary, eligibleSpill := map[string]bool{}, map[string]bool{}
	m.mu.RLock()
	for host, enclave := range m.Enclaves {
		if enclave == failed || host == failed.host || !enclave.breakerClosed() || !enclave.attestationValid(time.Now()) {
			continue
		}
		if overloaded, _, _ := enclave.ShouldReject(); overloaded {
			continue
		}
		if primary == nil || primary[host] {
			eligiblePrimary[host] = true
		} else if spill[host] {
			eligibleSpill[host] = true
		}
	}
	m.mu.RUnlock()
	alternative, _ := m.selectForDispatchPools(order, false, eligiblePrimary, eligibleSpill)
	if alternative == nil || !alternative.breakerClosed() {
		return nil
	}
	if overloaded, _, _ := alternative.ShouldReject(); overloaded {
		return nil
	}
	return alternative
}

// retryBadRequest returns only an accepted alternate response. Rejected attempts
// leave the original response intact and cannot recursively retry. Neither
// response bodies nor request fields are logged or added to metrics.
func (m *Model) retryBadRequest(failed *Enclave, original *http.Response, order []string) (*Enclave, *http.Response) {
	if !canRetryBadRequest(original) {
		return nil, nil
	}
	alternative := m.badRequestAlternative(failed, original.Request, order)
	if alternative == nil {
		return nil, nil
	}
	body, err := original.Request.GetBody()
	if err != nil {
		return nil, nil
	}
	ctx := context.WithValue(original.Request.Context(), badRequestFallbackKey{}, (*badRequestFallback)(nil))
	ctx = WithProbeClaim(ctx, nil)
	if labels, ok := ctx.Value(tokenLabelsKey{}).(tokenLabels); ok {
		primary, spill := m.ReservationPools(CallerOrgFromContext(ctx))
		labels.pool = "none"
		if primary != nil {
			labels.pool = "shared"
			if spill != nil {
				labels.pool = "reserved"
				if !primary[alternative.host] {
					labels.pool = "spilled"
				}
			}
		}
		ctx = WithTokenMetricLabels(ctx, labels.pool, labels.priority)
	}
	req := original.Request.Clone(ctx)
	req.Body = body
	req.URL.Host = alternative.host
	req.Host = alternative.host
	req.RequestURI = ""
	alternative.inflight.Add(1)
	BackendInflight.WithLabelValues(alternative.modelName, alternative.host).Inc()
	resp, err := alternative.proxy.Transport.RoundTrip(req)
	if err != nil {
		body.Close()
		alternative.inflight.Add(-1)
		BackendInflight.WithLabelValues(alternative.modelName, alternative.host).Dec()
		reason := classifyProxyError(err)
		if reason == "canceled" {
			ClientCancellationsTotal.WithLabelValues(alternative.modelName, alternative.host).Inc()
		} else {
			ProxyFailureTotal.WithLabelValues(alternative.modelName, alternative.host, reason).Inc()
			if alternative.cb != nil {
				alternative.cb.RecordFailure()
				publishBreakerState(alternative.modelName, alternative.host, alternative.cb)
			}
		}
		return nil, nil
	}
	resp.Body = &inflightBody{ReadCloser: resp.Body, enclave: alternative}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		recordHTTPOutcome(alternative.modelName, alternative.host, alternative.cb, resp)
		resp.Body.Close()
		return nil, nil
	}
	original.Body.Close()
	if failed.cb != nil && failed.cb.Quarantine() {
		publishBreakerState(failed.modelName, failed.host, failed.cb)
		log.WithFields(log.Fields{"model": failed.modelName, "enclave": failed.host, "alternate": alternative.host}).Warn("quarantined backend after another replica accepted its rejected request")
	}
	return alternative, resp
}
