package toolruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/toolcontext"
	"github.com/tinfoilsh/confidential-model-router/toolprofile"
)

const maxToolIterations = 5

type headerRoundTripper struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	for key, values := range t.headers {
		for _, value := range values {
			if value != "" {
				cloned.Header.Add(key, value)
			}
		}
	}
	return t.base.RoundTrip(cloned)
}

func Handle(w http.ResponseWriter, r *http.Request, em *manager.EnclaveManager, profile toolprofile.Profile, body map[string]any, modelName string) error {
	ctx := r.Context()
	requestHeaders := modelRequestHeaders(r.Header)
	session, err := connectToolSession(ctx, em, profile, r, modelName, body)
	if err != nil {
		return err
	}
	defer session.Close()

	toolsResult, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	promptResult, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: profile.PromptName})
	if err != nil {
		return err
	}
	ownedTools := ownedToolNames(toolsResult.Tools)

	switch r.URL.Path {
	case "/v1/chat/completions":
		response, err := runChatLoop(ctx, em, session, body, modelName, requestHeaders, promptResult, toolsResult.Tools, ownedTools)
		if err != nil {
			return writeUpstreamError(w, err)
		}
		if isStream(body) {
			return streamChatCompletion(w, r, response)
		}
		return writeJSONResponse(w, response)
	case "/v1/responses":
		response, err := runResponsesLoop(ctx, em, session, body, modelName, requestHeaders, promptResult, toolsResult.Tools, ownedTools)
		if err != nil {
			return writeUpstreamError(w, err)
		}
		if isStream(body) {
			return streamResponses(w, response)
		}
		return writeJSONResponse(w, response)
	default:
		return fmt.Errorf("unsupported tool runtime route: %s", r.URL.Path)
	}
}

func connectToolSession(ctx context.Context, em *manager.EnclaveManager, profile toolprofile.Profile, r *http.Request, modelName string, body map[string]any) (*mcp.ClientSession, error) {
	endpoint, httpClient, err := em.MCPServerEndpoint(profile.ToolServerModel)
	if err != nil {
		return nil, err
	}

	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = uuid.NewString()
	}

	headers := make(http.Header)
	headers.Set(toolcontext.HeaderRequestID, requestID)
	headers.Set(toolcontext.HeaderModel, modelName)
	headers.Set(toolcontext.HeaderRoute, r.URL.Path)
	headers.Set(toolcontext.HeaderStreaming, strconv.FormatBool(isStream(body)))
	if auth := r.Header.Get("Authorization"); auth != "" {
		headers.Set("Authorization", auth)
	}
	httpClient.Transport = &headerRoundTripper{
		base:    httpClient.Transport,
		headers: headers,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "router-tool-runtime", Version: "v1"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	}, nil)
}

func runChatLoop(ctx context.Context, em *manager.EnclaveManager, session *mcp.ClientSession, body map[string]any, modelName string, requestHeaders http.Header, prompt *mcp.GetPromptResult, tools []*mcp.Tool, ownedTools map[string]struct{}) (*upstreamJSONResponse, error) {
	reqBody := cloneJSONMap(body)
	delete(reqBody, "web_search_options")
	reqBody["stream"] = false
	reqBody["parallel_tool_calls"] = false
	reqBody["tools"] = append(existingTools(reqBody["tools"]), chatTools(tools)...)
	reqBody["messages"] = prependChatPrompt(prompt, reqBody["messages"])

	for i := 0; i < maxToolIterations; i++ {
		response, err := postJSON(ctx, em, modelName, "/v1/chat/completions", reqBody, requestHeaders)
		if err != nil {
			return nil, err
		}

		message, toolCalls := parseChatToolCalls(response.body)
		routerToolCalls, _ := splitToolCalls(ownedTools, toolCalls)
		if len(routerToolCalls) == 0 {
			return response, nil
		}

		messages, _ := reqBody["messages"].([]any)
		messages = append(messages, message)
		for _, toolCall := range routerToolCalls {
			output, err := callTool(ctx, session, toolCall.name, toolCall.arguments)
			if err != nil {
				output = err.Error()
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": toolCall.id,
				"content":      output,
			})
		}
		reqBody["messages"] = messages
	}

	return nil, fmt.Errorf("tool loop exceeded max iterations")
}

func runResponsesLoop(ctx context.Context, em *manager.EnclaveManager, session *mcp.ClientSession, body map[string]any, modelName string, requestHeaders http.Header, prompt *mcp.GetPromptResult, tools []*mcp.Tool, ownedTools map[string]struct{}) (*upstreamJSONResponse, error) {
	base := cloneJSONMap(body)
	base["stream"] = false
	base["parallel_tool_calls"] = false
	base["tools"] = replaceResponsesWebSearchTools(base["tools"], responseTools(tools))
	base["input"] = prependResponsesPrompt(prompt, base["input"])

	reqBody := base
	for i := 0; i < maxToolIterations; i++ {
		response, err := postJSON(ctx, em, modelName, "/v1/responses", reqBody, requestHeaders)
		if err != nil {
			return nil, err
		}

		routerToolCalls, _ := splitToolCalls(ownedTools, parseResponsesToolCalls(response.body))
		if len(routerToolCalls) == 0 {
			return response, nil
		}

		toolOutputs := make([]map[string]any, 0, len(routerToolCalls))
		for _, toolCall := range routerToolCalls {
			output, err := callTool(ctx, session, toolCall.name, toolCall.arguments)
			if err != nil {
				output = err.Error()
			}
			toolOutputs = append(toolOutputs, map[string]any{
				"type":    "function_call_output",
				"call_id": toolCall.id,
				"output":  output,
			})
		}

		reqBody = map[string]any{
			"model":                modelName,
			"previous_response_id": stringValue(response.body["id"]),
			"input":                toolOutputs,
			"tools":                base["tools"],
		}
	}

	return nil, fmt.Errorf("tool loop exceeded max iterations")
}

type upstreamJSONResponse struct {
	body       map[string]any
	header     http.Header
	statusCode int
}

type upstreamError struct {
	statusCode int
	header     http.Header
	body       []byte
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d: %s", e.statusCode, strings.TrimSpace(string(e.body)))
}

func postJSON(ctx context.Context, em *manager.EnclaveManager, modelName, path string, body map[string]any, requestHeaders http.Header) (*upstreamJSONResponse, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	reqHeaders := cloneHeaders(requestHeaders)
	reqHeaders.Set("Content-Type", "application/json")

	resp, err := em.DoModelRequest(ctx, modelName, path, bodyBytes, reqHeaders)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &upstreamError{
			statusCode: resp.StatusCode,
			header:     resp.Header.Clone(),
			body:       respBody,
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	return &upstreamJSONResponse{
		body:       parsed,
		header:     resp.Header.Clone(),
		statusCode: resp.StatusCode,
	}, nil
}

type toolCall struct {
	id        string
	name      string
	arguments map[string]any
}

func parseChatToolCalls(response map[string]any) (map[string]any, []toolCall) {
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, nil
	}
	rawCalls, _ := message["tool_calls"].([]any)
	toolCalls := make([]toolCall, 0, len(rawCalls))
	for _, rawCall := range rawCalls {
		callMap, _ := rawCall.(map[string]any)
		functionMap, _ := callMap["function"].(map[string]any)
		args := parseArguments(functionMap["arguments"])
		toolCalls = append(toolCalls, toolCall{
			id:        stringValue(callMap["id"]),
			name:      stringValue(functionMap["name"]),
			arguments: args,
		})
	}
	return message, toolCalls
}

func parseResponsesToolCalls(response map[string]any) []toolCall {
	output, _ := response["output"].([]any)
	var result []toolCall
	for _, item := range output {
		itemMap, _ := item.(map[string]any)
		itemType := stringValue(itemMap["type"])
		if itemType != "function_call" && itemType != "mcp_call" {
			continue
		}
		callID := stringValue(itemMap["call_id"])
		if callID == "" {
			callID = stringValue(itemMap["id"])
		}
		name := stringValue(itemMap["name"])
		args := parseArguments(itemMap["arguments"])
		result = append(result, toolCall{
			id:        callID,
			name:      name,
			arguments: args,
		})
	}
	return result
}

func parseArguments(raw any) map[string]any {
	switch value := raw.(type) {
	case string:
		var parsed map[string]any
		if json.Unmarshal([]byte(value), &parsed) == nil {
			return parsed
		}
	case map[string]any:
		return value
	}
	return map[string]any{}
}

func callTool(ctx context.Context, session *mcp.ClientSession, name string, arguments map[string]any) (string, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		return "", err
	}
	if result.StructuredContent != nil {
		body, err := json.Marshal(result.StructuredContent)
		if err == nil {
			return string(body), nil
		}
	}
	var parts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, textContent.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func ownedToolNames(tools []*mcp.Tool) map[string]struct{} {
	result := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name == "" {
			continue
		}
		result[tool.Name] = struct{}{}
	}
	return result
}

func splitToolCalls(ownedTools map[string]struct{}, toolCalls []toolCall) ([]toolCall, []toolCall) {
	routerToolCalls := make([]toolCall, 0, len(toolCalls))
	clientToolCalls := make([]toolCall, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if _, ok := ownedTools[toolCall.name]; ok {
			routerToolCalls = append(routerToolCalls, toolCall)
			continue
		}
		clientToolCalls = append(clientToolCalls, toolCall)
	}
	return routerToolCalls, clientToolCalls
}

func existingTools(raw any) []any {
	tools, _ := raw.([]any)
	if tools == nil {
		return []any{}
	}
	return tools
}

func chatTools(tools []*mcp.Tool) []any {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.InputSchema,
			},
		})
	}
	return result
}

func responseTools(tools []*mcp.Tool) []any {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		result = append(result, map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.InputSchema,
		})
	}
	return result
}

func replaceResponsesWebSearchTools(raw any, replacements []any) []any {
	tools, _ := raw.([]any)
	if len(tools) == 0 {
		return replacements
	}
	result := make([]any, 0, len(tools)+len(replacements))
	injected := false
	for _, tool := range tools {
		toolMap, _ := tool.(map[string]any)
		if stringValue(toolMap["type"]) == "web_search" {
			if !injected {
				result = append(result, replacements...)
				injected = true
			}
			continue
		}
		result = append(result, tool)
	}
	return result
}

func prependChatPrompt(prompt *mcp.GetPromptResult, raw any) []any {
	messages, _ := raw.([]any)
	promptMessages := promptMessages(prompt)
	if len(promptMessages) == 0 {
		return messages
	}
	return append(promptMessages, messages...)
}

func prependResponsesPrompt(prompt *mcp.GetPromptResult, raw any) any {
	items, ok := raw.([]any)
	if !ok {
		items = []any{raw}
	}
	promptItems := promptInputItems(prompt)
	if len(promptItems) == 0 {
		return items
	}
	return append(promptItems, items...)
}

func promptMessages(prompt *mcp.GetPromptResult) []any {
	if prompt == nil {
		return nil
	}
	result := make([]any, 0, len(prompt.Messages))
	for _, message := range prompt.Messages {
		textContent, ok := message.Content.(*mcp.TextContent)
		if !ok || strings.TrimSpace(textContent.Text) == "" {
			continue
		}
		result = append(result, map[string]any{
			"role":    message.Role,
			"content": textContent.Text,
		})
	}
	return result
}

func promptInputItems(prompt *mcp.GetPromptResult) []any {
	if prompt == nil {
		return nil
	}
	result := make([]any, 0, len(prompt.Messages))
	for _, message := range prompt.Messages {
		textContent, ok := message.Content.(*mcp.TextContent)
		if !ok || strings.TrimSpace(textContent.Text) == "" {
			continue
		}
		result = append(result, map[string]any{
			"type": "message",
			"role": message.Role,
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": textContent.Text,
				},
			},
		})
	}
	return result
}

func isStream(body map[string]any) bool {
	stream, _ := body["stream"].(bool)
	return stream
}

func modelRequestHeaders(source http.Header) http.Header {
	headers := make(http.Header)
	for _, key := range []string{"Authorization", manager.UsageMetricsRequestHeader, "X-Tinfoil-Client-Requested-Usage"} {
		for _, value := range source.Values(key) {
			headers.Add(key, value)
		}
	}
	return headers
}

func cloneHeaders(source http.Header) http.Header {
	headers := make(http.Header, len(source))
	for key, values := range source {
		copied := make([]string, len(values))
		copy(copied, values)
		headers[key] = copied
	}
	return headers
}

func writeUpstreamError(w http.ResponseWriter, err error) error {
	upstreamErr, ok := err.(*upstreamError)
	if !ok {
		return err
	}

	copyResponseHeaders(w.Header(), upstreamErr.header)
	if contentType := upstreamErr.header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(upstreamErr.body)))
	w.WriteHeader(upstreamErr.statusCode)
	_, writeErr := w.Write(upstreamErr.body)
	return writeErr
}

func writeJSONResponse(w http.ResponseWriter, response *upstreamJSONResponse) error {
	data, err := json.Marshal(response.body)
	if err != nil {
		return err
	}

	copyResponseHeaders(w.Header(), response.header)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(response.statusCode)
	_, err = w.Write(data)
	return err
}

func streamChatCompletion(w http.ResponseWriter, r *http.Request, response *upstreamJSONResponse) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	copyResponseHeaders(w.Header(), response.header)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(response.statusCode)

	choices, _ := response.body["choices"].([]any)
	if len(choices) == 0 {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	id := stringValue(response.body["id"])
	created := int64(numberValue(response.body["created"]))
	model := stringValue(response.body["model"])

	if err := sseData(w, map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
				},
			},
		},
	}); err != nil {
		return err
	}

	if content := stringValue(message["content"]); content != "" {
		if err := sseData(w, map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"content": content,
					},
				},
			},
		}); err != nil {
			return err
		}
	}

	if rawToolCalls, _ := message["tool_calls"].([]any); len(rawToolCalls) > 0 {
		if err := sseData(w, map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": rawToolCalls,
					},
				},
			},
		}); err != nil {
			return err
		}
	}

	finishReason := stringValue(choice["finish_reason"])
	if finishReason == "" {
		if rawToolCalls, _ := message["tool_calls"].([]any); len(rawToolCalls) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}

	if err := sseData(w, map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
	}); err != nil {
		return err
	}

	if r.Header.Get("X-Tinfoil-Client-Requested-Usage") == "true" && response.body["usage"] != nil {
		if err := sseData(w, map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   response.body["usage"],
		}); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func streamResponses(w http.ResponseWriter, response *upstreamJSONResponse) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	copyResponseHeaders(w.Header(), response.header)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(response.statusCode)

	responseBody := cloneJSONMap(response.body)
	id := stringValue(responseBody["id"])
	if id == "" {
		id = "resp_" + uuid.NewString()
		responseBody["id"] = id
	}

	createdAt := int64(numberValue(responseBody["created_at"]))
	if createdAt == 0 {
		createdAt = int64(numberValue(responseBody["created"]))
	}

	if err := sseEvent(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         id,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      stringValue(responseBody["model"]),
			"output":     []any{},
		},
	}); err != nil {
		return err
	}

	output, _ := responseBody["output"].([]any)
	for index, item := range output {
		itemMap, _ := item.(map[string]any)
		if err := sseEvent(w, "response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": index,
			"item":         itemMap,
		}); err != nil {
			return err
		}
		if err := emitResponseContentEvents(w, itemMap, index); err != nil {
			return err
		}
		if err := sseEvent(w, "response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": index,
			"item":         itemMap,
		}); err != nil {
			return err
		}
	}

	if err := sseEvent(w, "response.completed", map[string]any{
		"type":     "response.completed",
		"response": responseBody,
	}); err != nil {
		return err
	}

	flusher.Flush()
	return nil
}

func emitResponseContentEvents(w http.ResponseWriter, item map[string]any, outputIndex int) error {
	itemID := stringValue(item["id"])
	contentItems, _ := item["content"].([]any)
	for contentIndex, rawContent := range contentItems {
		contentMap, _ := rawContent.(map[string]any)
		text := stringValue(contentMap["text"])
		if text == "" {
			continue
		}

		eventType := responseContentDeltaEvent(contentMap)
		if err := sseEvent(w, eventType, map[string]any{
			"type":          eventType,
			"output_index":  outputIndex,
			"item_id":       itemID,
			"content_index": contentIndex,
			"delta":         text,
		}); err != nil {
			return err
		}

		if eventType == "response.output_text.delta" {
			if err := sseEvent(w, "response.output_text.done", map[string]any{
				"type":          "response.output_text.done",
				"output_index":  outputIndex,
				"item_id":       itemID,
				"content_index": contentIndex,
				"text":          text,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func responseContentDeltaEvent(content map[string]any) string {
	switch stringValue(content["type"]) {
	case "reasoning_text":
		return "response.reasoning_text.delta"
	default:
		return "response.output_text.delta"
	}
}

func sseData(w http.ResponseWriter, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func sseEvent(w http.ResponseWriter, event string, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if shouldSkipResponseHeader(key) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldSkipResponseHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func cloneJSONMap(in map[string]any) map[string]any {
	data, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func numberValue(v any) float64 {
	f, _ := v.(float64)
	return f
}
