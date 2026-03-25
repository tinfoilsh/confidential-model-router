package toolexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

const (
	codeInterpreterToolName = "code_interpreter"
)

type usageAccumulator struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
}

func (u *usageAccumulator) Add(usage *tokencount.Usage) {
	if usage == nil {
		return
	}
	usage.Normalize()
	u.PromptTokens += usage.PromptTokens
	u.CompletionTokens += usage.CompletionTokens
	if usage.TotalTokens > 0 {
		u.TotalTokens += usage.TotalTokens
	} else {
		u.TotalTokens += usage.PromptTokens + usage.CompletionTokens
	}
	u.InputTokens += usage.InputTokens
	u.OutputTokens += usage.OutputTokens
}

func (u *usageAccumulator) ToUsage() *tokencount.Usage {
	if u == nil {
		return nil
	}
	return &tokencount.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
	}
}

func deepCopyMap(src map[string]any) (map[string]any, error) {
	if src == nil {
		return nil, nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("deepCopyMap marshal: %w", err)
	}
	var dst map[string]any
	if err := json.Unmarshal(raw, &dst); err != nil {
		return nil, fmt.Errorf("deepCopyMap unmarshal: %w", err)
	}
	return dst, nil
}

func writeAPIError(w http.ResponseWriter, message string, errType string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
		},
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	return json.NewEncoder(w).Encode(payload)
}

func forwardResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if len(body) > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(resp.StatusCode)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func requestIDFromResponse(resp *http.Response) string {
	if id := resp.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	return resp.Header.Get("X-Request-ID")
}

func extractUsageFromBody(body []byte) *tokencount.Usage {
	if len(body) == 0 {
		return nil
	}
	var payload struct {
		Usage *tokencount.Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Usage == nil {
		return nil
	}
	payload.Usage.Normalize()
	return payload.Usage
}

func usageMetricsRequested(req *http.Request) bool {
	return req != nil && req.Header.Get(manager.UsageMetricsRequestHeader) == "true"
}

func bearerToken(req *http.Request) string {
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
}

func clientRequestedStreamingUsage(req *http.Request) bool {
	return req != nil && req.Header.Get("X-Tinfoil-Client-Requested-Usage") == "true"
}

func setUsageHeaderOrTrailer(w http.ResponseWriter, req *http.Request, usage *tokencount.Usage) {
	if !usageMetricsRequested(req) || usage == nil {
		return
	}
	w.Header().Set(manager.UsageMetricsResponseHeader, manager.FormatUsage(usage))
}

func prepareUsageTrailer(w http.ResponseWriter, req *http.Request) {
	if usageMetricsRequested(req) {
		manager.AddTrailerHeader(w.Header(), manager.UsageMetricsResponseHeader)
	}
}

func ensureResponsesInputItems(input any) []any {
	switch value := input.(type) {
	case nil:
		return []any{}
	case string:
		return []any{
			map[string]any{
				"role":    "user",
				"content": value,
			},
		}
	case []any:
		return append([]any(nil), value...)
	default:
		return []any{value}
	}
}

func findStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if ok {
			result = append(result, text)
		}
	}
	return result
}

func jsonString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// toolSchema returns the code_interpreter function definition for the Chat
// Completions API, which wraps the function spec inside a "function" object:
//
//	{"type": "function", "function": {"name": ..., "parameters": ...}}
func toolSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        codeInterpreterToolName,
			"description": codeInterpreterDescription,
			"strict":      true,
			"parameters":  codeInterpreterParameters(),
		},
	}
}

// responsesToolSchema returns the code_interpreter function definition for the
// Responses API, which uses a flat layout (no "function" wrapper):
//
//	{"type": "function", "name": ..., "parameters": ...}
func responsesToolSchema() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        codeInterpreterToolName,
		"description": codeInterpreterDescription,
		"strict":      true,
		"parameters":  codeInterpreterParameters(),
	}
}

const codeInterpreterDescription = "Execute Python code in a reusable sandbox container. Use this when code execution would help answer the user."

func codeInterpreterParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "Python code to execute inside the reusable sandbox container.",
			},
			"container_id": map[string]any{
				"type":        "string",
				"description": "Optional container ID to reuse an existing sandbox session.",
			},
		},
		"required":             []string{"code"},
		"additionalProperties": false,
	}
}

func rawJSONMap(value any) map[string]any {
	if existing, ok := value.(map[string]any); ok {
		return existing
	}
	return nil
}

func rawJSONArray(value any) []any {
	if existing, ok := value.([]any); ok {
		return existing
	}
	return nil
}

func compactJSONString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		payload, _ := json.Marshal(typed)
		return string(payload)
	}
}

func isUsageOnlyChatChunk(chunk map[string]any) bool {
	usageValue, ok := chunk["usage"]
	if !ok || usageValue == nil {
		return false
	}
	choices, ok := chunk["choices"].([]any)
	return ok && len(choices) == 0
}

func collectChunkUsage(chunk map[string]any) *tokencount.Usage {
	usageValue, ok := chunk["usage"]
	if !ok || usageValue == nil {
		return nil
	}
	raw, err := json.Marshal(usageValue)
	if err != nil {
		return nil
	}
	var usage tokencount.Usage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil
	}
	usage.Normalize()
	return &usage
}

func copyResponseMap(body []byte) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode JSON response: %w", err)
	}
	return payload, nil
}

func encodeSSE(data map[string]any) ([]byte, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	eventType := jsonString(data["type"])
	if eventType != "" {
		buf.WriteString("event: ")
		buf.WriteString(eventType)
		buf.WriteString("\n")
	}
	buf.WriteString("data: ")
	buf.Write(payload)
	buf.WriteString("\n\n")
	return buf.Bytes(), nil
}
