package toolruntime

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

// finalize emits any remaining buffered citations, forwards client-owned
// tool_calls the model asked the user to execute, writes the finish-reason
// chunk and final usage chunk, fires the billing event, and closes the
// stream with `[DONE]`.
func (s *chatStreamer) finalize(
	r *http.Request,
	em *manager.EnclaveManager,
	modelName string,
	result chatIterationResult,
	clientToolCalls []toolCall,
) error {
	// Defensive: in the degenerate case where upstream never emitted a
	// single chunk before signaling done (and the pump therefore never
	// ran ensureRoleEmitted), the finish chunk below would be the
	// client's first visible delta; still emit role first to keep the
	// protocol contract.
	s.ensureRoleEmitted()
	s.flushCitations()

	if len(clientToolCalls) > 0 {
		raw := rawToolCallsFromParsed(clientToolCalls, result.rawToolCalls)
		s.writeChunk(map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": raw},
				},
			},
		})
	}

	finishReason := result.finishReason
	if finishReason == "" {
		if len(clientToolCalls) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}
	s.writeChunk(map[string]any{
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
	})

	finalUsage := s.finalUsage()
	if s.clientRequestedUsage && finalUsage != nil {
		s.writeChunk(map[string]any{
			"choices": []any{},
			"usage":   finalUsage,
		})
	}

	if s.writeErr == nil {
		if _, err := io.WriteString(s.w, "data: [DONE]\n\n"); err != nil {
			s.writeErr = err
		} else {
			s.flusher.Flush()
		}
	}

	if s.usageMetricsRequested && finalUsage != nil {
		s.w.Header().Set(manager.UsageMetricsResponseHeader, formatUsageHeader(usageFromRawMap(finalUsage)))
	}
	s.emitBillingEvent(r, em, modelName, finalUsage)
	return s.writeErr
}

// finalUsage assembles the aggregated usage block across every upstream
// iteration in the OpenAI Chat Completions shape. The underlying
// accumulator already handles turns where upstream omitted the usage block
// entirely by falling back to zero.
func (s *chatStreamer) finalUsage() map[string]any {
	usage := s.usageTotals.Usage()
	if usage == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
	}
}

// emitBillingEvent delegates to the shared billing emitter used by the
// non-streaming path so streaming and non-streaming requests produce the
// same billing-event shape, including Tinfoil-Enclave attribution and
// request-id resolution from upstream headers.
func (s *chatStreamer) emitBillingEvent(r *http.Request, em *manager.EnclaveManager, modelName string, usage map[string]any) {
	if em == nil {
		return
	}
	header := s.upstreamHeaders
	if header == nil {
		header = http.Header{}
	}
	response := &upstreamJSONResponse{
		header: header,
		body:   map[string]any{"usage": usage},
	}
	emitBillingEvent(em, r, response, modelName, true)
}

// terminateWithError is the single point at which a mid-stream upstream
// failure is surfaced to the client. If no SSE has started yet we return
// the upstream error to the caller so Handle turns it into a plain JSON
// error response; otherwise we emit a top-level error data frame in the
// shape OpenAI SDKs recognize mid-stream, followed by `[DONE]` so SDKs can
// close their streams cleanly.
func (s *chatStreamer) terminateWithError(r *http.Request, em *manager.EnclaveManager, modelName string, err error) error {
	if !s.headersWritten {
		return err
	}
	s.flushCitations()
	s.emitData(map[string]any{"error": upstreamErrorPayload(err)})
	if s.writeErr == nil {
		if _, werr := io.WriteString(s.w, "data: [DONE]\n\n"); werr != nil {
			s.writeErr = werr
		} else {
			s.flusher.Flush()
		}
	}
	s.emitBillingEvent(r, em, modelName, s.finalUsage())
	return nil
}

// upstreamErrorPayload extracts the most informative structured error
// object we can show the client. If the underlying error is an
// *upstreamError whose body is JSON with an `error` field, we return that
// object verbatim (preserving the upstream's own `code`, `type`, `param`,
// etc.). Otherwise we fall back to a minimal `{message, type}` envelope.
func upstreamErrorPayload(err error) map[string]any {
	if upErr, ok := err.(*upstreamError); ok && len(upErr.body) > 0 {
		var parsed map[string]any
		if json.Unmarshal(upErr.body, &parsed) == nil {
			if inner, ok := parsed["error"].(map[string]any); ok {
				return inner
			}
		}
	}
	return map[string]any{
		"message": err.Error(),
		"type":    "upstream_error",
	}
}
