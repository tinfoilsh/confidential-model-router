package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

type genericRequest struct {
	Model string
	Body  map[string]any
	Raw   []byte
}

type RequestPlan struct {
	runner  *Runner
	typed   *Request
	generic *genericRequest
}

func (r *Runner) PlanRequest(httpReq *http.Request, body []byte) (*RequestPlan, error) {
	if httpReq == nil {
		return nil, fmt.Errorf("request is required")
	}

	if typedReq, handledPath, err := ParseRequest(httpReq.URL.Path, httpReq.Header, body); err != nil {
		return nil, err
	} else if handledPath {
		return &RequestPlan{
			runner: r,
			typed:  typedReq,
		}, nil
	}

	genericReq, err := parseGenericRequest(body)
	if err != nil {
		return nil, err
	}
	return &RequestPlan{
		runner:  r,
		generic: genericReq,
	}, nil
}

func (p *RequestPlan) Serve(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, em *manager.EnclaveManager, apiKey string) {
	if p == nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
		return
	}
	if p.typed != nil {
		p.serveTyped(ctx, w, httpReq, em, apiKey)
		return
	}
	p.serveGeneric(w, httpReq, em, apiKey)
}

func (p *RequestPlan) serveTyped(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, em *manager.EnclaveManager, apiKey string) {
	if p.typed == nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
		return
	}

	rateLimitModel := p.typed.Model
	p.typed.StripPriority()
	if shouldDemotePriority(em, apiKey, rateLimitModel) {
		p.typed.SetPriority(1)
	}

	if p.typed.Stream {
		if err := p.typed.EnableContinuousUsageStats(); err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
			return
		}
		if p.typed.ClientRequestedStreamingUsage {
			httpReq.Header.Set("X-Tinfoil-Client-Requested-Usage", "true")
		}
		log.Debugf("Modified streaming request body to include continuous_usage_stats, client requested usage: %v", p.typed.ClientRequestedStreamingUsage)
	}

	activeTool, err := p.runner.Prepare(p.typed)
	if err != nil {
		writeAPIError(w, err.Error(), manager.ErrTypeInvalidRequest, http.StatusBadRequest)
		return
	}

	bodyBytes, err := p.typed.BodyBytes()
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
		return
	}

	effectiveModel := p.typed.Model
	if activeTool != nil && activeTool.Prepared != nil {
		if len(activeTool.Prepared.Body) > 0 {
			bodyBytes = activeTool.Prepared.Body
		}
		if activeTool.Prepared.EffectiveModel != "" {
			effectiveModel = activeTool.Prepared.EffectiveModel
		}
	}

	if activeTool == nil {
		serveProxyModel(w, httpReq, em, effectiveModel, bodyBytes)
		return
	}

	if _, ok := activeTool.Executor(); !ok {
		serveProxyModel(w, httpReq, em, effectiveModel, bodyBytes)
		return
	}

	invoker, err := em.NewUpstreamInvoker(effectiveModel)
	if err != nil {
		switch {
		case errors.Is(err, manager.ErrNoEnclaveAvailable("")):
			writeAPIError(w, manager.ErrMsgOverloaded, manager.ErrTypeServer, http.StatusServiceUnavailable)
		case errors.Is(err, manager.ErrModelNotFound("")):
			writeAPIError(w, manager.ErrMsgModelNotFound, manager.ErrTypeInvalidRequest, http.StatusNotFound)
		default:
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
		}
		return
	}
	if rejectOverloadedEnclave(w, invoker.Enclave(), effectiveModel) {
		return
	}

	resetRequestBody(httpReq, bodyBytes)
	if p.typed.Stream {
		err = p.runner.HandleStream(ctx, w, httpReq, p.typed, activeTool, invoker)
	} else {
		err = p.runner.HandleJSON(ctx, w, httpReq, p.typed, activeTool, invoker)
	}
	if err != nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
	}
}

func (p *RequestPlan) serveGeneric(w http.ResponseWriter, httpReq *http.Request, em *manager.EnclaveManager, apiKey string) {
	if p.generic == nil {
		writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
		return
	}

	body := p.generic.Body
	rateLimitModel := p.generic.Model

	_, hadPriority := body["priority"]
	delete(body, "priority")
	bodyModified := hadPriority

	if shouldDemotePriority(em, apiKey, rateLimitModel) {
		body["priority"] = 1
		bodyModified = true
	}

	if stream, ok := body["stream"].(bool); ok && stream {
		clientRequestedUsage := false
		if streamOptions, ok := body["stream_options"].(map[string]any); ok {
			if includeUsage, ok := streamOptions["include_usage"].(bool); ok && includeUsage {
				clientRequestedUsage = true
			}
			if continuousUsage, ok := streamOptions["continuous_usage_stats"].(bool); ok && continuousUsage {
				clientRequestedUsage = true
			}
			streamOptions["continuous_usage_stats"] = true
		} else {
			body["stream_options"] = map[string]any{
				"continuous_usage_stats": true,
			}
		}
		if clientRequestedUsage {
			httpReq.Header.Set("X-Tinfoil-Client-Requested-Usage", "true")
		}
		bodyModified = true
		log.Debugf("Modified streaming request body to include continuous_usage_stats, client requested usage: %v", clientRequestedUsage)
	}

	bodyBytes := p.generic.Raw
	if bodyModified {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusInternalServerError)
			return
		}
	}

	serveProxyModel(w, httpReq, em, p.generic.Model, bodyBytes)
}

func parseGenericRequest(body []byte) (*genericRequest, error) {
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	modelValue, ok := fields["model"]
	if !ok {
		return nil, fmt.Errorf("missing required parameter: 'model'")
	}
	model, ok := modelValue.(string)
	if !ok {
		return nil, fmt.Errorf("invalid parameter: 'model' must be a string")
	}
	return &genericRequest{
		Model: model,
		Body:  fields,
		Raw:   append([]byte(nil), body...),
	}, nil
}

func shouldDemotePriority(em *manager.EnclaveManager, apiKey string, model string) bool {
	if em == nil || apiKey == "" || model == "" {
		return false
	}
	rlCfg := em.GetRateLimitConfig(model)
	if rlCfg == nil {
		return false
	}
	if !em.RequestTracker().RecordAndCheck(apiKey, model, rlCfg.MaxRequestsPerMinute) {
		return false
	}
	manager.RateLimitDemotionsTotal.WithLabelValues(model).Inc()
	log.WithFields(log.Fields{
		"model": model,
	}).Debug("rate limited: injecting lower vLLM priority")
	return true
}

func serveProxyModel(w http.ResponseWriter, httpReq *http.Request, em *manager.EnclaveManager, modelName string, body []byte) {
	model, found := em.GetModel(modelName)
	if !found {
		writeAPIError(w, manager.ErrMsgModelNotFound, manager.ErrTypeInvalidRequest, http.StatusNotFound)
		return
	}

	enclave := model.NextEnclave()
	if enclave == nil {
		writeAPIError(w, manager.ErrMsgOverloaded, manager.ErrTypeServer, http.StatusServiceUnavailable)
		return
	}
	if rejectOverloadedEnclave(w, enclave, modelName) {
		return
	}

	log.Debugf("%s serving request\n", enclave)
	resetRequestBody(httpReq, body)
	enclave.ServeHTTP(w, httpReq)
}

func rejectOverloadedEnclave(w http.ResponseWriter, enclave *manager.Enclave, modelName string) bool {
	if enclave == nil {
		return false
	}
	if overloaded, retryAfter, waiting := enclave.ShouldReject(); overloaded {
		secs := int(retryAfter.Seconds())
		if secs <= 0 {
			secs = 60
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		log.WithFields(log.Fields{
			"model":               modelName,
			"enclave":             enclave.String(),
			"requests_waiting":    waiting,
			"retry_after_seconds": secs,
		}).Warn("rejecting request due to backend overload")

		manager.RequestsRejectedTotal.WithLabelValues(modelName).Inc()
		manager.RetryAfterSeconds.WithLabelValues(modelName).Observe(float64(secs))

		writeAPIError(w, fmt.Sprintf("Request rate exceeded. Retry after %d seconds.", secs), manager.ErrTypeInvalidRequest, http.StatusTooManyRequests)
		return true
	}
	return false
}
