package toolexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/toolexec/codeinterpreter"
)

const maxResponsesToolIterations = 8

type normalizedResponsesRequest struct {
	body           map[string]any
	session        *codeinterpreter.Session
	includeOutputs bool
}

type processedResponsesOutput struct {
	publicItems          []any
	functionCallOutputs  []any
	executedAny          bool
	mixedWithUserTools   bool
	codeInterpreterItems []map[string]any
}

type ciStreamCall struct {
	ItemID      string
	OutputIndex int
	CallID      string
	Code        string
}

func (e *Executor) handleResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, body map[string]any, invoker *manager.UpstreamInvoker) error {
	normalized, err := normalizeResponsesRequest(body)
	if err != nil {
		return err
	}

	if normalized.body["stream"] == true {
		return e.handleResponsesStream(ctx, w, r, normalized, invoker)
	}
	return e.handleResponsesJSON(ctx, w, r, normalized, invoker)
}

func normalizeResponsesRequest(body map[string]any) (*normalizedResponsesRequest, error) {
	tools := append([]any(nil), rawJSONArray(body["tools"])...)

	// Single-pass collision check: reject if both a builtin {type:"code_interpreter"}
	// and a user function named "code_interpreter" exist simultaneously.
	hasBuiltinCI := false
	hasFunctionCI := false
	for _, toolValue := range tools {
		tool := rawJSONMap(toolValue)
		if tool == nil {
			continue
		}
		if jsonString(tool["type"]) == codeInterpreterToolName {
			hasBuiltinCI = true
		}
		if jsonString(tool["type"]) == "function" && toolHasName(tool, codeInterpreterToolName) {
			hasFunctionCI = true
		}
	}
	if hasBuiltinCI && hasFunctionCI {
		return nil, fmt.Errorf("tool name collision with reserved tool %q", codeInterpreterToolName)
	}

	var (
		config     codeinterpreter.ContainerConfig
		foundCI    bool
		includeOut bool
		rewritten  []any
	)

	for _, toolValue := range tools {
		tool := rawJSONMap(toolValue)
		if tool == nil {
			rewritten = append(rewritten, toolValue)
			continue
		}

		if toolHasName(tool, codeInterpreterToolName) && jsonString(tool["type"]) == "function" {
			rewritten = append(rewritten, toolValue)
			continue
		}

		if jsonString(tool["type"]) != codeInterpreterToolName {
			rewritten = append(rewritten, toolValue)
			continue
		}

		if foundCI {
			return nil, fmt.Errorf("only one code_interpreter tool is supported in this increment")
		}
		foundCI = true

		containerValue, hasContainer := tool["container"]
		if !hasContainer {
			containerValue = map[string]any{"type": "auto"}
		}
		parsed, err := parseContainerConfig(containerValue, true)
		if err != nil {
			return nil, err
		}
		config = parsed
	}

	if !foundCI {
		return nil, fmt.Errorf("code_interpreter tool is required")
	}

	for _, include := range findStringSlice(body["include"]) {
		if include == "code_interpreter_call.outputs" {
			includeOut = true
			break
		}
	}

	session, err := codeinterpreter.NewSession(config)
	if err != nil {
		return nil, err
	}

	normalized, err := deepCopyMap(body)
	if err != nil {
		return nil, err
	}
	rewritten = append(rewritten, responsesToolSchema())
	normalized["tools"] = rewritten

	// The Responses API backend only supports string tool_choice values
	// ("auto", "none", "required").  Any object/map value (e.g.
	// {type:"code_interpreter"} or {type:"function",name:"get_weather"}) is
	// rejected.  Strip them so the backend defaults to "auto"; the model will
	// still call the appropriate tool based on the prompt and tool definitions.
	if _, isMap := normalized["tool_choice"].(map[string]any); isMap {
		delete(normalized, "tool_choice")
	}

	return &normalizedResponsesRequest{
		body:           normalized,
		session:        session,
		includeOutputs: includeOut,
	}, nil
}

func (e *Executor) handleResponsesJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, normalized *normalizedResponsesRequest, invoker *manager.UpstreamInvoker) error {
	w.Header().Set("Tinfoil-Enclave", invoker.Enclave().String())

	requestBody, err := deepCopyMap(normalized.body)
	if err != nil {
		return err
	}
	internalInput := ensureResponsesInputItems(requestBody["input"])
	requestBody["input"] = internalInput

	publicOutput := []any{}
	usage := &usageAccumulator{}
	var finalPayload map[string]any

	for range maxResponsesToolIterations {
		bodyBytes, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		resp, err := invoker.Do(ctx, r.Method, r.URL.Path, r.Header, bodyBytes)
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
		invoker.RecordUsage(r, requestIDFromResponse(resp), currentUsage, false)

		if resp.StatusCode >= http.StatusBadRequest {
			forwardResponse(w, resp, responseBody)
			return nil
		}

		payload, err := copyResponseMap(responseBody)
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
			return nil
		}

		outputItems := append([]any(nil), rawJSONArray(payload["output"])...)
		processed, err := e.processResponsesOutput(ctx, outputItems, normalized.session, normalized.includeOutputs, bearerToken(r))
		if err != nil {
			writeAPIError(w, manager.ErrMsgServerError, manager.ErrTypeServer, http.StatusBadGateway)
			return nil
		}

		publicOutput = append(publicOutput, processed.publicItems...)
		finalPayload = payload

		if !processed.executedAny {
			if len(publicOutput) > 0 {
				finalPayload["output"] = publicOutput
			}
			finalPayload["usage"] = usage.ToUsage()
			setUsageHeaderOrTrailer(w, r, usage.ToUsage())
			return writeJSON(w, http.StatusOK, finalPayload)
		}

		if processed.mixedWithUserTools {
			finalPayload["output"] = publicOutput
			finalPayload["usage"] = usage.ToUsage()
			setUsageHeaderOrTrailer(w, r, usage.ToUsage())
			return writeJSON(w, http.StatusOK, finalPayload)
		}

		internalInput = append(internalInput, outputItems...)
		internalInput = append(internalInput, processed.functionCallOutputs...)
		requestBody["input"] = internalInput
	}

	return fmt.Errorf("max tool iterations exceeded")
}

func (e *Executor) processResponsesOutput(ctx context.Context, outputItems []any, session *codeinterpreter.Session, includeOutputs bool, apiKey string) (*processedResponsesOutput, error) {
	result := &processedResponsesOutput{}
	for _, itemValue := range outputItems {
		item := rawJSONMap(itemValue)
		if jsonString(item["type"]) != "function_call" || jsonString(item["name"]) != codeInterpreterToolName {
			result.publicItems = append(result.publicItems, itemValue)
			if jsonString(item["type"]) == "function_call" {
				result.mixedWithUserTools = true
			}
			continue
		}

		execResult, err := e.codeInterpreter.Execute(ctx, jsonString(item["call_id"]), jsonString(item["arguments"]), session, apiKey)
		if err != nil {
			return nil, err
		}

		result.executedAny = true
		publicItem := map[string]any{
			"id":           firstNonEmpty(jsonString(item["id"]), execResult.ID),
			"type":         "code_interpreter_call",
			"code":         execResult.Code,
			"container_id": execResult.ContainerID,
			"status":       execResult.Status,
		}
		if includeOutputs {
			publicItem["outputs"] = outputsToAny(execResult.Outputs)
		}
		result.publicItems = append(result.publicItems, publicItem)
		result.codeInterpreterItems = append(result.codeInterpreterItems, publicItem)

		toolOutputPayload := map[string]any{
			"status":       execResult.Status,
			"container_id": execResult.ContainerID,
			"outputs":      outputsToAny(execResult.Outputs),
			"exit_code":    execResult.ExitCode,
		}
		result.functionCallOutputs = append(result.functionCallOutputs, map[string]any{
			"type":    "function_call_output",
			"call_id": jsonString(item["call_id"]),
			"output":  compactJSONString(toolOutputPayload),
		})
	}
	return result, nil
}

func (e *Executor) handleResponsesStream(ctx context.Context, w http.ResponseWriter, r *http.Request, normalized *normalizedResponsesRequest, invoker *manager.UpstreamInvoker) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Tinfoil-Enclave", invoker.Enclave().String())
	prepareUsageTrailer(w, r)

	flusher, _ := w.(http.Flusher)

	requestBody, err := deepCopyMap(normalized.body)
	if err != nil {
		return err
	}
	internalInput := ensureResponsesInputItems(requestBody["input"])
	requestBody["input"] = internalInput

	publicOutput := []any{}
	usage := &usageAccumulator{}
	var sequence int64 = 1

	for range maxResponsesToolIterations {
		bodyBytes, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		resp, err := invoker.Do(ctx, r.Method, r.URL.Path, r.Header, bodyBytes)
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
			completed    map[string]any
			hadCIThisHop bool
			streamCalls  = map[string]ciStreamCall{}
			requestID    = requestIDFromResponse(resp)
		)

		err = parseSSEStream(resp.Body, func(event map[string]any) error {
			eventType := jsonString(event["type"])
			switch eventType {
			case "response.output_item.added":
				item := rawJSONMap(event["item"])
				if jsonString(item["type"]) == "function_call" && jsonString(item["name"]) == codeInterpreterToolName {
					hadCIThisHop = true
					outputIndex := int(numberValue(event["output_index"]))
					itemID := jsonString(item["id"])
					streamCalls[itemID] = ciStreamCall{
						ItemID:      itemID,
						OutputIndex: outputIndex,
						CallID:      jsonString(item["call_id"]),
					}
					return emitResponsesEvent(w, &sequence, map[string]any{
						"type":         "response.output_item.added",
						"output_index": outputIndex,
						"item": map[string]any{
							"id":     firstNonEmpty(itemID, jsonString(item["call_id"])),
							"type":   "code_interpreter_call",
							"status": codeinterpreter.StatusInProgress,
						},
					}, flusher)
				}
			case "response.function_call_arguments.delta":
				if _, ok := streamCalls[jsonString(event["item_id"])]; ok {
					hadCIThisHop = true
					return nil
				}
			case "response.function_call_arguments.done":
				itemID := jsonString(event["item_id"])
				call, ok := streamCalls[itemID]
				if ok {
					hadCIThisHop = true
					args, err := codeinterpreter.ParseArgs(jsonString(event["arguments"]))
					if err != nil {
						return err
					}
					call.Code = args.Code
					streamCalls[itemID] = call
					if err := emitResponsesEvent(w, &sequence, map[string]any{
						"type":         "response.code_interpreter_call_code.delta",
						"item_id":      itemID,
						"output_index": call.OutputIndex,
						"delta":        args.Code,
					}, flusher); err != nil {
						return err
					}
					return emitResponsesEvent(w, &sequence, map[string]any{
						"type":         "response.code_interpreter_call_code.done",
						"item_id":      itemID,
						"output_index": call.OutputIndex,
						"code":         args.Code,
					}, flusher)
				}
			case "response.output_item.done":
				item := rawJSONMap(event["item"])
				if jsonString(item["type"]) == "function_call" && jsonString(item["name"]) == codeInterpreterToolName {
					hadCIThisHop = true
					return nil
				}
			case "response.completed":
				completed = rawJSONMap(event["response"])
				if completed != nil {
					eventUsage := collectChunkUsage(completed)
					usage.Add(eventUsage)
					invoker.RecordUsage(r, requestID, eventUsage, true)
				}
				if !hadCIThisHop && len(publicOutput) == 0 {
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

		if !hadCIThisHop {
			if len(publicOutput) == 0 {
				setUsageHeaderOrTrailer(w, r, usage.ToUsage())
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
			setUsageHeaderOrTrailer(w, r, usage.ToUsage())
			return nil
		}

		outputItems := append([]any(nil), rawJSONArray(completed["output"])...)
		processed, err := e.processResponsesOutput(ctx, outputItems, normalized.session, normalized.includeOutputs, bearerToken(r))
		if err != nil {
			return err
		}

		publicOutput = append(publicOutput, processed.publicItems...)
		internalInput = append(internalInput, outputItems...)
		internalInput = append(internalInput, processed.functionCallOutputs...)

		for _, item := range processed.codeInterpreterItems {
			itemID := jsonString(item["id"])
			call := streamCalls[itemID]
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":         "response.code_interpreter_call.in_progress",
				"item_id":      itemID,
				"output_index": call.OutputIndex,
			}, flusher); err != nil {
				return err
			}
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":         "response.code_interpreter_call.interpreting",
				"item_id":      itemID,
				"output_index": call.OutputIndex,
			}, flusher); err != nil {
				return err
			}
			if err := emitResponsesEvent(w, &sequence, map[string]any{
				"type":         "response.code_interpreter_call.completed",
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
			setUsageHeaderOrTrailer(w, r, usage.ToUsage())
			return nil
		}

		requestBody["input"] = internalInput
	}

	return fmt.Errorf("max tool iterations exceeded")
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
