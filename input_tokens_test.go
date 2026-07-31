package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestHandleInputTokensChatRequest(t *testing.T) {
	var dispatchedModel string
	var dispatchedPath string
	var dispatchedBody map[string]any
	var dispatchedAuthorization string
	dispatch := func(_ context.Context, modelName, path string, body []byte, headers http.Header) (*http.Response, error) {
		dispatchedModel = modelName
		dispatchedPath = path
		dispatchedAuthorization = headers.Get("Authorization")
		if err := json.Unmarshal(body, &dispatchedBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"count":37,"tokens":[1,2,3]}`)),
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, chatInputTokensPath, strings.NewReader(`{
		"model":"gpt-oss-120b",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"max_completion_tokens":100
	}`))
	rec := httptest.NewRecorder()
	handleInputTokens(rec, req, "secret-key", "", nil, dispatch)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if dispatchedModel != "gpt-oss-120b" || dispatchedPath != tokenizePath {
		t.Fatalf("unexpected dispatch target: model=%q path=%q", dispatchedModel, dispatchedPath)
	}
	if dispatchedAuthorization != "Bearer secret-key" {
		t.Fatalf("unexpected authorization header %q", dispatchedAuthorization)
	}
	if _, ok := dispatchedBody["max_completion_tokens"]; ok {
		t.Fatal("max_completion_tokens must not be sent to /tokenize")
	}
	if _, ok := dispatchedBody["messages"].([]any); !ok {
		t.Fatalf("expected messages in tokenize body: %#v", dispatchedBody)
	}

	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "chat.completion.input_tokens" || response["input_tokens"] != float64(37) {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestHandleInputTokensResponsesRequest(t *testing.T) {
	var dispatchedBody map[string]any
	dispatch := func(_ context.Context, _, _ string, body []byte, _ http.Header) (*http.Response, error) {
		if err := json.Unmarshal(body, &dispatchedBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"count":91}`)),
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, responsesInputTokensPath, strings.NewReader(`{
		"model":"gpt-oss-120b",
		"instructions":"Be concise.",
		"input":[{
			"role":"user",
			"content":[
				{"type":"input_text","text":"Describe this."},
				{"type":"input_image","image_url":"https://example.com/image.png","detail":"low"}
			]
		}],
		"tools":[{
			"type":"function",
			"name":"lookup",
			"description":"Look something up",
			"parameters":{"type":"object"},
			"strict":true
		}]
	}`))
	rec := httptest.NewRecorder()
	handleInputTokens(rec, req, "secret-key", "", nil, dispatch)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	expectedMessages := []any{
		map[string]any{"role": "system", "content": "Be concise."},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "Describe this."},
				map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url":    "https://example.com/image.png",
						"detail": "low",
					},
				},
			},
		},
	}
	if !reflect.DeepEqual(dispatchedBody["messages"], expectedMessages) {
		t.Fatalf("unexpected converted messages:\nwant: %#v\n got: %#v", expectedMessages, dispatchedBody["messages"])
	}
	expectedTools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "lookup",
			"description": "Look something up",
			"parameters":  map[string]any{"type": "object"},
			"strict":      true,
		},
	}}
	if !reflect.DeepEqual(dispatchedBody["tools"], expectedTools) {
		t.Fatalf("unexpected converted tools:\nwant: %#v\n got: %#v", expectedTools, dispatchedBody["tools"])
	}
	if !strings.Contains(rec.Body.String(), `"object":"response.input_tokens"`) {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

func TestResponsesMessagesConvertsFunctionCalls(t *testing.T) {
	messages, err := responsesMessages(map[string]any{
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "first", "arguments": `{}`},
			map[string]any{"type": "function_call", "call_id": "call_2", "name": "second", "arguments": `{"id":1}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected grouped assistant calls and one tool result, got %#v", messages)
	}
	assistant := messages[0].(map[string]any)
	if calls := assistant["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("expected two grouped tool calls, got %#v", calls)
	}
	tool := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "done" {
		t.Fatalf("unexpected tool output message: %#v", tool)
	}
}

func TestResponsesMessagesAcceptsNullInstructionsAndRefusal(t *testing.T) {
	messages, err := responsesMessages(map[string]any{
		"instructions": nil,
		"input": []any{map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]any{
				"type":    "refusal",
				"refusal": "I cannot help with that.",
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected one message, got %#v", messages)
	}
	message := messages[0].(map[string]any)
	content := message["content"].([]any)
	want := map[string]any{"type": "text", "text": "I cannot help with that."}
	if !reflect.DeepEqual(content[0], want) {
		t.Fatalf("unexpected refusal conversion: %#v", content[0])
	}
}

func TestHandleInputTokensRequiresBearerKey(t *testing.T) {
	dispatched := false
	dispatch := func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
		dispatched = true
		return nil, nil
	}
	req := httptest.NewRequest(http.MethodPost, responsesInputTokensPath, strings.NewReader(`{"model":"gpt-oss-120b","input":"hello"}`))
	rec := httptest.NewRecorder()

	handleInputTokens(rec, req, "", "", nil, dispatch)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if dispatched {
		t.Fatal("unauthenticated request was dispatched")
	}
}

func TestHandleInputTokensUsesSubdomainModel(t *testing.T) {
	var dispatchedModel string
	dispatch := func(_ context.Context, modelName, _ string, _ []byte, _ http.Header) (*http.Response, error) {
		dispatchedModel = modelName
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"count":5}`)),
		}, nil
	}
	req := httptest.NewRequest(http.MethodPost, chatInputTokensPath, strings.NewReader(`{
		"messages":[{"role":"user","content":"hello"}]
	}`))
	rec := httptest.NewRecorder()

	handleInputTokens(rec, req, "secret-key", "subdomain-model", nil, dispatch)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if dispatchedModel != "subdomain-model" {
		t.Fatalf("expected subdomain model, got %q", dispatchedModel)
	}
}

func TestHandleInputTokensForwardsTokenizeError(t *testing.T) {
	dispatch := func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: http.StatusUnprocessableEntity,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid messages"}}`)),
		}, nil
	}
	req := httptest.NewRequest(http.MethodPost, chatInputTokensPath, strings.NewReader(`{"model":"gpt-oss-120b","messages":[]}`))
	rec := httptest.NewRecorder()

	handleInputTokens(rec, req, "secret-key", "", nil, dispatch)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
	if rec.Body.String() != `{"error":{"message":"invalid messages"}}` {
		t.Fatalf("unexpected forwarded body: %s", rec.Body.String())
	}
}
