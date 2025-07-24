package tokencount

import (
	"bytes"
	"io"
	"net/http"
	"testing"
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

			// Extract tokens
			newBody, usage, err := ExtractTokensFromResponse(resp, "test-model")
			if (err != nil) != tt.wantErr {
				t.Errorf("ExtractTokensFromResponse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Check usage
			if tt.wantUsage != nil && usage != nil {
				if usage.PromptTokens != tt.wantUsage.PromptTokens ||
					usage.CompletionTokens != tt.wantUsage.CompletionTokens ||
					usage.TotalTokens != tt.wantUsage.TotalTokens {
					t.Errorf("ExtractTokensFromResponse() usage = %+v, want %+v", usage, tt.wantUsage)
				}
			} else if (tt.wantUsage == nil) != (usage == nil) {
				t.Errorf("ExtractTokensFromResponse() usage = %+v, want %+v", usage, tt.wantUsage)
			}

			// Verify we can still read the response body
			bodyBytes, err := io.ReadAll(newBody)
			if err != nil {
				t.Errorf("Failed to read new body: %v", err)
			}
			if string(bodyBytes) != tt.responseBody {
				t.Errorf("Body content changed: got %s, want %s", string(bodyBytes), tt.responseBody)
			}
		})
	}
}