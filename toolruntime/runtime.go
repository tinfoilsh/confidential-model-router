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
	authHeader := r.Header.Get("Authorization")
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
		reqBody, response, err := runChatLoop(ctx, em, session, body, modelName, authHeader, promptResult, toolsResult.Tools, ownedTools)
		if err != nil {
			return err
		}
		if isStream(body) {
			reqBody["stream"] = true
			return streamModelResponse(ctx, w, em, modelName, r.URL.Path, reqBody, authHeader)
		}
		return writeJSON(w, response)
	case "/v1/responses":
		reqBody, response, err := runResponsesLoop(ctx, em, session, body, modelName, authHeader, promptResult, toolsResult.Tools, ownedTools)
		if err != nil {
			return err
		}
		if isStream(body) {
			reqBody["stream"] = true
			return streamModelResponse(ctx, w, em, modelName, r.URL.Path, reqBody, authHeader)
		}
		return writeJSON(w, response)
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

func runChatLoop(ctx context.Context, em *manager.EnclaveManager, session *mcp.ClientSession, body map[string]any, modelName string, authHeader string, prompt *mcp.GetPromptResult, tools []*mcp.Tool, ownedTools map[string]struct{}) (map[string]any, map[string]any, error) {
	reqBody := cloneJSONMap(body)
	delete(reqBody, "web_search_options")
	reqBody["stream"] = false
	reqBody["parallel_tool_calls"] = false
	reqBody["tools"] = append(existingTools(reqBody["tools"]), chatTools(tools)...)
	reqBody["messages"] = prependChatPrompt(prompt, reqBody["messages"])

	for i := 0; i < maxToolIterations; i++ {
		response, err := postJSON(ctx, em, modelName, "/v1/chat/completions", reqBody, authHeader)
		if err != nil {
			return nil, nil, err
		}

		message, toolCalls := parseChatToolCalls(response)
		routerToolCalls, _ := splitToolCalls(ownedTools, toolCalls)
		if len(routerToolCalls) == 0 {
			return reqBody, response, nil
		}

		messages, _ := reqBody["messages"].([]any)
		if filteredMessage := filterChatToolCalls(message, routerToolCalls); filteredMessage != nil {
			messages = append(messages, filteredMessage)
		}
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

	return nil, nil, fmt.Errorf("tool loop exceeded max iterations")
}

func runResponsesLoop(ctx context.Context, em *manager.EnclaveManager, session *mcp.ClientSession, body map[string]any, modelName string, authHeader string, prompt *mcp.GetPromptResult, tools []*mcp.Tool, ownedTools map[string]struct{}) (map[string]any, map[string]any, error) {
	base := cloneJSONMap(body)
	base["stream"] = false
	base["tools"] = replaceResponsesWebSearchTools(base["tools"], responseTools(tools))
	base["input"] = prependResponsesPrompt(prompt, base["input"])

	reqBody := base
	for i := 0; i < maxToolIterations; i++ {
		response, err := postJSON(ctx, em, modelName, "/v1/responses", reqBody, authHeader)
		if err != nil {
			return nil, nil, err
		}

		routerToolCalls, _ := splitToolCalls(ownedTools, parseResponsesToolCalls(response))
		if len(routerToolCalls) == 0 {
			return reqBody, response, nil
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
			"previous_response_id": stringValue(response["id"]),
			"input":                toolOutputs,
			"tools":                base["tools"],
		}
	}

	return nil, nil, fmt.Errorf("tool loop exceeded max iterations")
}

func postJSON(ctx context.Context, em *manager.EnclaveManager, modelName, path string, body map[string]any, authHeader string) (map[string]any, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	reqHeaders := make(http.Header)
	reqHeaders.Set("Content-Type", "application/json")
	if authHeader != "" {
		reqHeaders.Set("Authorization", authHeader)
	}

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
		return nil, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed map[string]any
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
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

func filterChatToolCalls(message map[string]any, allowed []toolCall) map[string]any {
	if message == nil || len(allowed) == 0 {
		return nil
	}

	allowedIDs := make(map[string]struct{}, len(allowed))
	for _, toolCall := range allowed {
		allowedIDs[toolCall.id] = struct{}{}
	}

	rawCalls, _ := message["tool_calls"].([]any)
	filteredCalls := make([]any, 0, len(allowed))
	for _, rawCall := range rawCalls {
		callMap, _ := rawCall.(map[string]any)
		if _, ok := allowedIDs[stringValue(callMap["id"])]; ok {
			filteredCalls = append(filteredCalls, rawCall)
		}
	}
	if len(filteredCalls) == 0 {
		return nil
	}

	filteredMessage := make(map[string]any, len(message))
	for key, value := range message {
		filteredMessage[key] = value
	}
	filteredMessage["tool_calls"] = filteredCalls
	return filteredMessage
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

func writeJSON(w http.ResponseWriter, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(data)
	return err
}

func streamModelResponse(ctx context.Context, w http.ResponseWriter, em *manager.EnclaveManager, modelName, path string, body map[string]any, authHeader string) error {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	reqHeaders := make(http.Header)
	reqHeaders.Set("Content-Type", "application/json")
	if authHeader != "" {
		reqHeaders.Set("Authorization", authHeader)
	}

	resp, err := em.DoModelRequest(ctx, modelName, path, bodyBytes, reqHeaders)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return readErr
		}
		return fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
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
