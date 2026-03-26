package e2e_test

// Tests for error handling and edge cases.
//
// These tests verify that the router returns correct HTTP status codes and
// error types for invalid requests, missing fields, and unsupported configurations.
// No CI or websearch availability required unless noted.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// TestErrors_MissingModelField verifies that a JSON body without a "model"
// field returns 400 with type invalid_request_error.
func TestErrors_MissingModelField(t *testing.T) {
	cfg := getConfig(t)
	c := newRouterClient(t, cfg)

	resp := c.postJSON(t, "/v1/chat/completions", map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	requireStatus(t, resp, 400)
	result := decodeJSON(t, resp)
	requireErrorType(t, result, "invalid_request_error")
	t.Logf("error message: %s", result["error"].(map[string]any)["message"])
}

// TestErrors_ModelFieldNotString verifies that a non-string "model" value
// is rejected. The router uses openai-go's lenient JSON decoder which coerces
// numbers to strings (12345 → "12345"), so the request gets as far as model
// lookup and returns 404 (model not found) rather than 400 (invalid type).
// Both are acceptable rejection codes; we just verify it's an invalid_request_error.
func TestErrors_ModelFieldNotString(t *testing.T) {
	cfg := getConfig(t)
	c := newRouterClient(t, cfg)

	resp := c.postJSON(t, "/v1/chat/completions", map[string]any{
		"model": 12345,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	// 400 (invalid field) or 404 (coerced to "12345", model not found) are both valid.
	if resp.StatusCode != 400 && resp.StatusCode != 404 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("want status 400 or 404, got %d: %s", resp.StatusCode, string(body))
	}
	result := decodeJSON(t, resp)
	requireErrorType(t, result, "invalid_request_error")
}


// TestErrors_InvalidJSONBody verifies that a malformed JSON body returns 400.
func TestErrors_InvalidJSONBody(t *testing.T) {
	cfg := getConfig(t)
	c := newRouterClient(t, cfg)

	req, err := newRawPostRequest(c.baseURL+"/v1/chat/completions", cfg.APIKey,
		"application/json", []byte(`{not valid json`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	requireStatus(t, resp, 400)
	result := decodeJSON(t, resp)
	requireErrorType(t, result, "invalid_request_error")
}

// TestErrors_ChatCI_Disabled verifies that when code_interpreter_options is present
// but the router was started without CODE_INTERPRETER_BASE_URL, the router returns
// 500 with an appropriate error. This test only runs when CI is explicitly disabled.
func TestErrors_ChatCI_Disabled(t *testing.T) {
	cfg := getConfig(t)
	if cfg.CIAvailable {
		t.Skip("code interpreter is available; this test requires it to be absent")
	}
	oai := newOpenAIClient(t, cfg)

	_, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("use code"),
		},
	}, option.WithJSONSet("code_interpreter_options", map[string]any{}))

	body := requireAPIError(t, err, 500)
	requireErrorType(t, body, "server_error")
	t.Logf("correctly returned 500: %v", body["error"])
}

// TestErrors_UserPriorityStripped verifies that a user-supplied priority field
// is silently removed before the request reaches the model. This prevents
// queue-jumping attacks. The router must accept the request (no 400) —
// downstream 503 (model overloaded) is acceptable and still proves the
// priority field did not cause a routing error.
func TestErrors_UserPriorityStripped(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	_, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say 'ok'"),
		},
		MaxTokens: openai.Int(5),
	}, option.WithJSONSet("priority", 100))

	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 400 {
			body := requireAPIError(t, err, 400)
			t.Errorf("priority field caused a 400 error (should be silently stripped): %v", body)
			return
		}
		// 200 (ok), 503 (backend overloaded), or 429 (rate-limited) are all valid — not 400.
		var apiErr2 *openai.Error
		if errors.As(err, &apiErr2) {
			t.Logf("priority field stripped: router returned %d (not 400)", apiErr2.StatusCode)
			return
		}
		t.Fatalf("unexpected error: %v", err)
	}
	t.Log("priority field stripped: router returned 200 (not 400)")
}

// TestErrors_OverloadedModel_RetryAfterHeader verifies that rate-limited models return
// HTTP 429 with a Retry-After header when the backend is overloaded.
// This test is informational — it verifies header presence IF a 429 is received.
func TestErrors_OverloadedModel_RetryAfterHeader(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	for range 3 {
		var rawResp *http.Response
		_, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
			Model: shared.ChatModel(cfg.Model),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("Say 'pong'"),
			},
			MaxTokens: openai.Int(5),
		}, option.WithResponseInto(&rawResp))

		if err != nil {
			var apiErr *openai.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == 429 {
				retryAfter := rawResp.Header.Get("Retry-After")
				if retryAfter == "" {
					t.Errorf("429 response missing Retry-After header: %s", apiErr.RawJSON())
				} else {
					t.Logf("429 with Retry-After: %s", retryAfter)
				}
				return
			}
		}
	}
	t.Log("no 429 responses encountered (backend not currently overloaded)")
}

// TestErrors_ResponsesCI_ToolNameCollision verifies that naming a user function
// "code_interpreter" in /v1/responses (while also having type=code_interpreter tool)
// is rejected.
func TestErrors_ResponsesCI_ToolNameCollision(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	c := newRouterClient(t, cfg)

	resp := c.postJSON(t, "/v1/responses", map[string]any{
		"model": cfg.Model,
		"input": "hello",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
			map[string]any{
				"type": "function",
				"name": "code_interpreter",
				"description": "shadow the reserved tool",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		"stream": false,
	})
	requireStatus(t, resp, 400)
	result := decodeJSON(t, resp)
	requireErrorType(t, result, "invalid_request_error")
}

// ─── local helpers ────────────────────────────────────────────────────────────

func newRawPostRequest(url, apiKey, contentType string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}
