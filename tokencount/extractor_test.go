package tokencount

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExtractTokensFromResponse(t *testing.T) {
	tests := []struct {
		name         string
		responseBody string
		contentType  string
		statusCode   int
		wantUsage    *Usage
		wantErr      bool
	}{
		{
			name: "valid OpenAI response",
			responseBody: `{
				"id": "chatcmpl-123",
				"object": "chat.completion",
				"created": 1677652288,
				"choices": [{
					"index": 0,
					"message": {"role": "assistant", "content": "Hello!"},
					"finish_reason": "stop"
				}],
				"usage": {
					"prompt_tokens": 10,
					"completion_tokens": 20,
					"total_tokens": 30
				}
			}`,
			contentType: "application/json",
			statusCode:  http.StatusOK,
			wantUsage: &Usage{
				PromptTokens:     10,
				CompletionTokens: 20,
				TotalTokens:      30,
			},
		},
		{
			name: "response without usage",
			responseBody: `{
				"id": "chatcmpl-123",
				"object": "chat.completion",
				"created": 1677652288,
				"choices": [{
					"index": 0,
					"message": {"role": "assistant", "content": "Hello!"},
					"finish_reason": "stop"
				}]
			}`,
			contentType: "application/json",
			statusCode:  http.StatusOK,
			wantUsage:   nil,
		},
		{
			name:         "non-JSON response",
			responseBody: `Hello, this is plain text`,
			contentType:  "text/plain",
			statusCode:   http.StatusOK,
			wantUsage:    nil,
		},
		{
			name:         "error response",
			responseBody: `{"error": "Invalid request"}`,
			contentType:  "application/json",
			statusCode:   http.StatusBadRequest,
			wantUsage:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock response
			resp := &http.Response{
				StatusCode: tt.statusCode,
				Header:     http.Header{"Content-Type": []string{tt.contentType}},
				Body:       io.NopCloser(bytes.NewReader([]byte(tt.responseBody))),
			}

			var capturedUsage *Usage
			usageHandler := func(usage *Usage) {
				capturedUsage = usage
			}

			// Extract tokens with handler
			newBody, _, err := ExtractTokensFromResponseWithHandler(resp, "test-model", usageHandler, false)
			if (err != nil) != tt.wantErr {
				t.Errorf("ExtractTokensFromResponseWithHandler() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Verify we can still read the response body
			bodyBytes, err := io.ReadAll(newBody)
			if err != nil {
				t.Errorf("Failed to read new body: %v", err)
			}
			if string(bodyBytes) != tt.responseBody {
				t.Errorf("Body content changed: got %s, want %s", string(bodyBytes), tt.responseBody)
			}
			
			// Close to trigger token extraction
			newBody.Close()
			
			// Verify usage was captured correctly
			if tt.wantUsage != nil {
				if capturedUsage == nil {
					t.Errorf("Expected usage to be captured, but it wasn't")
				} else if *capturedUsage != *tt.wantUsage {
					t.Errorf("Usage mismatch: got %+v, want %+v", capturedUsage, tt.wantUsage)
				}
			}
		})
	}
}

func TestStreamingTokenExtraction(t *testing.T) {
	tests := []struct {
		name           string
		streamData     string
		expectedUsage  *Usage
		expectCallback bool
	}{
		{
			name: "OpenAI streaming with usage in final chunk",
			streamData: `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}

data: [DONE]

`,
			expectedUsage: &Usage{
				PromptTokens:     10,
				CompletionTokens: 2,
				TotalTokens:      12,
			},
			expectCallback: true,
		},
		{
			name: "Streaming with continuous usage stats",
			streamData: `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}],"usage":{"prompt_tokens":5,"completion_tokens":0,"total_tokens":5}}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}

data: [DONE]

`,
			expectedUsage: &Usage{
				PromptTokens:     5,
				CompletionTokens: 1,
				TotalTokens:      6,
			},
			expectCallback: true,
		},
		{
			name: "Streaming without explicit usage (no callback expected)",
			streamData: `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":"This is a test"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`,
			expectedUsage:  nil, // No usage data means no billing
			expectCallback: false, // Callback should not be called without real usage data
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock streaming response
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tt.streamData)),
			}

			var capturedUsage *Usage
			callbackCalled := false
			usageHandler := func(usage *Usage) {
				capturedUsage = usage
				callbackCalled = true
			}

			// Extract tokens
			newBody, _, err := ExtractTokensFromResponseWithHandler(resp, "test-model", usageHandler, false)
			if err != nil {
				t.Errorf("ExtractTokensFromResponseWithHandler() error = %v", err)
				return
			}

			// Read the entire response to trigger processing
			output := &bytes.Buffer{}
			io.Copy(output, newBody)
			newBody.Close()

			// Give the background goroutine time to complete
			time.Sleep(100 * time.Millisecond)

			// Verify output matches input
			if output.String() != tt.streamData {
				t.Errorf("Stream output doesn't match input.\nGot:\n%s\nWant:\n%s", output.String(), tt.streamData)
			}

			// Verify callback was called if expected
			if tt.expectCallback && !callbackCalled {
				t.Errorf("Expected usage callback to be called, but it wasn't")
			}

			// Verify usage matches expected
			if tt.expectedUsage != nil && capturedUsage != nil {
				if capturedUsage.PromptTokens != tt.expectedUsage.PromptTokens ||
					capturedUsage.CompletionTokens != tt.expectedUsage.CompletionTokens ||
					capturedUsage.TotalTokens != tt.expectedUsage.TotalTokens {
					t.Errorf("Usage mismatch: got %+v, want %+v", capturedUsage, tt.expectedUsage)
				}
			}
		})
	}
}

func TestStreamingErrorCases(t *testing.T) {
	tests := []struct {
		name       string
		streamData string
	}{
		{
			name: "Malformed JSON in stream",
			streamData: `data: {"id":"chatcmpl-123","malformed json

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: [DONE]

`,
		},
		{
			name: "Empty data lines",
			streamData: `data: 

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: 

data: [DONE]

`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tt.streamData)),
			}

			// Should handle errors gracefully
			newBody, _, err := ExtractTokensFromResponse(resp, "test-model")
			if err != nil {
				t.Errorf("ExtractTokensFromResponse() unexpected error = %v", err)
				return
			}

			// Should still pass through the entire stream
			output := &bytes.Buffer{}
			io.Copy(output, newBody)
			newBody.Close()

			if output.String() != tt.streamData {
				t.Errorf("Stream output doesn't match input despite errors")
			}
		})
	}
}