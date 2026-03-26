package openaiapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	oairesponses "github.com/openai/openai-go/v3/responses"
	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

const maxResponsesToolIterations = 12

type processedResponsesOutput struct {
	publicItems         []any
	replayItems         []any
	executedAny         bool
	mixedWithUserTools  bool
	executedPublicItems []map[string]any
}

type toolStreamCall struct {
	ItemID      string
	OutputIndex int
	CallID      string
	Code        string
}

func (r *Runner) handleResponsesJSON(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, active *ActiveTool, invoker *manager.UpstreamInvoker) error {
	cleanupCtx, cancelCleanup := cleanupContext(httpReq)
	defer cancelCleanup()
	defer func() {
		_ = closePreparedState(cleanupCtx, active.Prepared)
	}()

	w.Header().Set("Tinfoil-Enclave", invoker.Enclave().String())

	requestBody, err := copyResponseMap(active.Prepared.Body)
	if err != nil {
		return err
	}
	internalInput := ensureResponsesInputItems(requestBody["input"])
	requestBody["input"] = internalInput

	publicOutput := []any{}
	usage := &usageAccumulator{}
	execTool, _ := active.Executable()

	for range maxResponsesToolIterations {
		bodyBytes, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		resp, err := invoker.Do(ctx, httpReq.Method, httpReq.URL.Path, httpReq.Header, bodyBytes)
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
			return nil
		}

		responseBody, err := readResponseBody(resp)
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
			return nil
		}

		currentUsage := extractUsageFromBody(responseBody)
		usage.Add(currentUsage)
		invoker.RecordUsage(httpReq, requestIDFromResponse(resp), currentUsage, false)

		if resp.StatusCode >= http.StatusBadRequest {
			forwardResponse(w, resp, responseBody)
			return nil
		}

		var typedResponse oairesponses.Response
		if err := json.Unmarshal(responseBody, &typedResponse); err == nil && len(publicOutput) == 0 && !responsesHasToolCalls(&typedResponse, active.Tool.ID()) {
			setUsageHeaderOrTrailer(w, httpReq, usage.ToUsage())
			forwardResponse(w, resp, responseBody)
			return nil
		}

		payload, err := copyResponseMap(responseBody)
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
			return nil
		}

		outputItems := append([]any(nil), rawJSONArray(payload["output"])...)
		processed, err := processResponsesOutput(ctx, outputItems, active, execTool)
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
			return nil
		}

		publicOutput = append(publicOutput, processed.publicItems...)

		if !processed.executedAny {
			if len(publicOutput) > 0 {
				payload["output"] = publicOutput
			}
			payload["usage"] = usage.ToUsage()
			setUsageHeaderOrTrailer(w, httpReq, usage.ToUsage())
			return writeJSON(w, http.StatusOK, payload)
		}

		if processed.mixedWithUserTools {
			payload["output"] = publicOutput
			payload["usage"] = usage.ToUsage()
			setUsageHeaderOrTrailer(w, httpReq, usage.ToUsage())
			return writeJSON(w, http.StatusOK, payload)
		}

		internalInput = append(internalInput, outputItems...)
		internalInput = append(internalInput, processed.replayItems...)
		requestBody["input"] = internalInput
	}

	log.Warnf("responses: reached max tool iterations (%d), terminating", maxResponsesToolIterations)
	return fmt.Errorf("max tool iterations exceeded")
}

func processResponsesOutput(ctx context.Context, outputItems []any, active *ActiveTool, execTool ExecutableTool) (*processedResponsesOutput, error) {
	result := &processedResponsesOutput{}
	for _, itemValue := range outputItems {
		item := rawJSONMap(itemValue)
		if jsonString(item["type"]) != "function_call" || jsonString(item["name"]) != active.Tool.ID() {
			result.publicItems = append(result.publicItems, itemValue)
			if jsonString(item["type"]) == "function_call" {
				result.mixedWithUserTools = true
			}
			continue
		}

		raw, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		execResult, err := execTool.Execute(ctx, &InferenceToolCall{
			Endpoint: EndpointResponses,
			Raw:      raw,
			State:    active.Prepared.State,
		})
		if err != nil {
			return nil, err
		}
		if execResult == nil || execResult.ResponsesPublicItem == nil || execResult.ResponsesReplayItem == nil {
			return nil, fmt.Errorf("tool %s returned incomplete responses execution result", active.Tool.ID())
		}

		result.executedAny = true
		result.publicItems = append(result.publicItems, execResult.ResponsesPublicItem)
		result.executedPublicItems = append(result.executedPublicItems, execResult.ResponsesPublicItem)
		result.replayItems = append(result.replayItems, execResult.ResponsesReplayItem)
	}
	return result, nil
}

func (r *Runner) handleResponsesStream(ctx context.Context, w http.ResponseWriter, httpReq *http.Request, req *Request, active *ActiveTool, invoker *manager.UpstreamInvoker) error {
	cleanupCtx, cancelCleanup := cleanupContext(httpReq)
	defer cancelCleanup()
	defer func() {
		_ = closePreparedState(cleanupCtx, active.Prepared)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Tinfoil-Enclave", invoker.Enclave().String())
	prepareUsageTrailer(w, httpReq)

	flusher, _ := w.(http.Flusher)

	requestBody, err := copyResponseMap(active.Prepared.Body)
	if err != nil {
		return err
	}
	internalInput := ensureResponsesInputItems(requestBody["input"])
	requestBody["input"] = internalInput

	publicOutput := []any{}
	usage := &usageAccumulator{}
	var sequence int64 = 1
	execTool, _ := active.Executable()

	for range maxResponsesToolIterations {
		bodyBytes, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		resp, err := invoker.Do(ctx, httpReq.Method, httpReq.URL.Path, httpReq.Header, bodyBytes)
		if err != nil {
			return err
		}
		if resp.StatusCode >= http.StatusBadRequest {
			payload, _ := readResponseBody(resp)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(payload)
			return nil
		}

		var (
			completed      map[string]any
			hadToolThisHop bool
			streamCalls    = map[string]toolStreamCall{}
			requestID      = requestIDFromResponse(resp)
		)

		err = parseSSEStream(resp.Body, func(event map[string]any) error {
			eventType := jsonString(event["type"])
			switch eventType {
			case "response.output_item.added":
				item := rawJSONMap(event["item"])
				if jsonString(item["type"]) == "function_call" && jsonString(item["name"]) == active.Tool.ID() {
					hadToolThisHop = true
					outputIndex := int(numberValue(event["output_index"]))
					itemID := jsonString(item["id"])
					if itemID == "" {
						return nil // skip malformed events with missing item id
					}
					streamCalls[itemID] = toolStreamCall{
						ItemID:      itemID,
						OutputIndex: outputIndex,
						CallID:      jsonString(item["call_id"]),
					}
					return emitResponsesEvent(w, &sequence, map[string]any{
						"type":         "response.output_item.added",
						"output_index": outputIndex,
						"item": map[string]any{
							"id":     firstNonEmpty(itemID, jsonString(item["call_id"])),
							"type":   responsesToolOutputType(active.Tool.ID()),
							"status": "in_progress",
						},
					}, flusher)
				}
			case "response.function_call_arguments.delta":
				if _, ok := streamCalls[jsonString(event["item_id"])]; ok {
					hadToolThisHop = true
					return nil
				}
			case "response.function_call_arguments.done":
				itemID := jsonString(event["item_id"])
				call, ok := streamCalls[itemID]
				if ok {
					hadToolThisHop = true
					call.Code = extractCodeFromArguments(jsonString(event["arguments"]))
					streamCalls[itemID] = call
					if call.Code != "" {
						if err := emitResponsesEvent(w, &sequence, map[string]any{
							"type":         responsesToolCodeEventType(active.Tool.ID(), "delta"),
							"item_id":      itemID,
							"output_index": call.OutputIndex,
							"delta":        call.Code,
						}, flusher); err != nil {
							return err
						}
						return emitResponsesEvent(w, &sequence, map[string]any{
							"type":         responsesToolCodeEventType(active.Tool.ID(), "done"),
							"item_id":      itemID,
							"output_index": call.OutputIndex,
							"code":         call.Code,
						}, flusher)
					}
					return nil
				}
			case "response.output_item.done":
				item := rawJSONMap(event["item"])
				if jsonString(item["type"]) == "function_call" && jsonString(item["name"]) == active.Tool.ID() {
					hadToolThisHop = true
					return nil
				}
			case "response.completed":
				completed = rawJSONMap(event["response"])
				if completed != nil {
					eventUsage := collectChunkUsage(completed)
					usage.Add(eventUsage)
					invoker.RecordUsage(httpReq, requestID, eventUsage, true)
				}
				if !hadToolThisHop && len(publicOutput) == 0 {
					return emitResponsesEvent(w, &sequence, event, flusher)
				}
				return nil
			}
			return emitResponsesEvent(w, &sequence, event, flusher)
		})
		resp.Body.Close()
		if err != nil {
			return err
		}

		if !hadToolThisHop {
			if len(publicOutput) == 0 {
				setUsageHeaderOrTrailer(w, httpReq, usage.ToUsage())
				return nil
			}
			currentOutput := rawJSONArray(completed["output"])
			publicOutput = append(publicOutput, currentOutput...)
			completed["output"] = publicOutput
			completed["usage"] = usage.ToUsage()
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":     "response.completed",
				"response": completed,
			}, flusher); err != nil {
				return err
			}
			setUsageHeaderOrTrailer(w, httpReq, usage.ToUsage())
			return nil
		}

		outputItems := append([]any(nil), rawJSONArray(completed["output"])...)
		processed, err := processResponsesOutput(ctx, outputItems, active, execTool)
		if err != nil {
			return err
		}

		publicOutput = append(publicOutput, processed.publicItems...)
		internalInput = append(internalInput, outputItems...)
		internalInput = append(internalInput, processed.replayItems...)

		for _, item := range processed.executedPublicItems {
			itemID := jsonString(item["id"])
			call := streamCalls[itemID]
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":         responsesToolEventType(active.Tool.ID(), "in_progress"),
				"item_id":      itemID,
				"output_index": call.OutputIndex,
			}, flusher); err != nil {
				return err
			}
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":         responsesToolEventType(active.Tool.ID(), "interpreting"),
				"item_id":      itemID,
				"output_index": call.OutputIndex,
			}, flusher); err != nil {
				return err
			}
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":         responsesToolEventType(active.Tool.ID(), "completed"),
				"item_id":      itemID,
				"output_index": call.OutputIndex,
			}, flusher); err != nil {
				return err
			}
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":         "response.output_item.done",
				"output_index": call.OutputIndex,
				"item":         item,
			}, flusher); err != nil {
				return err
			}
		}

		if processed.mixedWithUserTools {
			completed["output"] = publicOutput
			completed["usage"] = usage.ToUsage()
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":     "response.completed",
				"response": completed,
			}, flusher); err != nil {
				return err
			}
			setUsageHeaderOrTrailer(w, httpReq, usage.ToUsage())
			return nil
		}

		requestBody["input"] = internalInput
	}

	log.Warnf("responses: reached max tool iterations (%d), terminating", maxResponsesToolIterations)
	return fmt.Errorf("max tool iterations exceeded")
}

func responsesHasToolCalls(resp *oairesponses.Response, toolName string) bool {
	if resp == nil {
		return false
	}
	for _, item := range resp.Output {
		if item.Type == "function_call" && strings.TrimSpace(item.Name) == toolName {
			return true
		}
	}
	return false
}

func emitResponsesEvent(w http.ResponseWriter, sequence *int64, event map[string]any, flusher http.Flusher) error {
	event, err := deepCopyMap(event)
	if err != nil {
		return err
	}
	event["sequence_number"] = *sequence
	*sequence++
	payload, err := encodeSSE(event)
	if err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func parseSSEStream(body io.Reader, onEvent func(map[string]any) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024), 16*1024*1024)

	var data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				var event map[string]any
				if err := json.Unmarshal(data.Bytes(), &event); err != nil {
					return err
				}
				if err := onEvent(event); err != nil {
					return err
				}
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if data.Len() > 0 {
		var event map[string]any
		if err := json.Unmarshal(data.Bytes(), &event); err != nil {
			return err
		}
		return onEvent(event)
	}
	return nil
}

func responsesToolOutputType(toolID string) string {
	return toolID + "_call"
}

func responsesToolEventType(toolID, state string) string {
	return "response." + responsesToolOutputType(toolID) + "." + state
}

func responsesToolCodeEventType(toolID, state string) string {
	return "response." + responsesToolOutputType(toolID) + "_code." + state
}

func extractCodeFromArguments(arguments string) string {
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(arguments), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Code)
}

func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
