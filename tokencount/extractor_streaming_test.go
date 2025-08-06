package tokencount

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamingTokenExtractor_ClientRequestedUsage(t *testing.T) {
	tests := []struct {
		name                 string
		clientRequestedUsage bool
		input                string
		expectedOutput       string
		description          string
	}{
		{
			name:                 "client_requested_usage_includes_usage_chunk",
			clientRequestedUsage: true,
			input: `data: {"id":"test","choices":[{"delta":{"content":"Hello"},"index":0}]}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: {"id":"test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
data: [DONE]
`,
			expectedOutput: `data: {"id":"test","choices":[{"delta":{"content":"Hello"},"index":0}]}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: {"id":"test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
data: [DONE]
`,
			description: "When client requests usage, all chunks including usage-only chunk should be passed through",
		},
		{
			name:                 "client_did_not_request_usage_filters_usage_chunk",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[{"delta":{"content":"Hello"},"index":0}]}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: {"id":"test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
data: [DONE]
`,
			expectedOutput: `data: {"id":"test","choices":[{"delta":{"content":"Hello"},"index":0}]}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: [DONE]
`,
			description: "When client didn't request usage, usage-only chunk (empty choices) should be filtered out",
		},
		{
			name:                 "usage_in_regular_chunk_always_passed",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[{"delta":{"content":"Hi"},"index":0}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: [DONE]
`,
			expectedOutput: `data: {"id":"test","choices":[{"delta":{"content":"Hi"},"index":0}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: [DONE]
`,
			description: "Usage data in chunks with non-empty choices should always be passed through",
		},
		{
			name:                 "multiple_usage_chunks_filtered",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[{"delta":{"content":"Test"},"index":0}]}
data: {"id":"test","choices":[],"usage":{"prompt_tokens":5}}
data: {"id":"test","choices":[{"delta":{"content":" response"},"index":0}]}
data: {"id":"test","choices":[],"usage":{"completion_tokens":10}}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: {"id":"test","choices":[],"usage":{"total_tokens":15}}
data: [DONE]
`,
			expectedOutput: `data: {"id":"test","choices":[{"delta":{"content":"Test"},"index":0}]}
data: {"id":"test","choices":[{"delta":{"content":" response"},"index":0}]}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}]}
data: [DONE]
`,
			description: "Multiple usage-only chunks should all be filtered when client didn't request usage",
		},
		{
			name:                 "non_sse_lines_passed_through",
			clientRequestedUsage: false,
			input: `event: ping
data: {"id":"test","choices":[{"delta":{"content":"Hello"},"index":0}]}

data: {"id":"test","choices":[],"usage":{"total_tokens":15}}
: comment line
data: [DONE]
`,
			expectedOutput: `event: ping
data: {"id":"test","choices":[{"delta":{"content":"Hello"},"index":0}]}

: comment line
data: [DONE]
`,
			description: "Non-data lines and empty lines should be passed through unchanged",
		},
		{
			name:                 "malformed_json_passed_through",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[{"delta":{"content":"OK"},"index":0}]}
data: {malformed json here
data: {"id":"test","choices":[],"usage":{"total_tokens":10}}
data: [DONE]
`,
			expectedOutput: `data: {"id":"test","choices":[{"delta":{"content":"OK"},"index":0}]}
data: {malformed json here
data: [DONE]
`,
			description: "Malformed JSON should be passed through, well-formed usage-only chunks filtered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create input reader and output buffer
			inputReader := io.NopCloser(strings.NewReader(tt.input))
			outputBuffer := &bytes.Buffer{}

			// Create pipe for output
			pr, pw := io.Pipe()

			// Create extractor
			extractor := NewStreamingTokenExtractor(inputReader, pw, "test-model")
			extractor.clientRequestedUsage = tt.clientRequestedUsage

			// Start processing in background
			processingDone := make(chan bool)
			go func() {
				extractor.processStream()
				processingDone <- true
			}()

			// Read output in background
			readDone := make(chan bool)
			go func() {
				io.Copy(outputBuffer, pr)
				readDone <- true
			}()

			// Wait for processing to complete
			<-processingDone

			// Wait for reading to complete
			<-readDone

			// Compare output
			actualOutput := outputBuffer.String()
			if actualOutput != tt.expectedOutput {
				t.Errorf("%s\nExpected output:\n%s\nActual output:\n%s",
					tt.description, tt.expectedOutput, actualOutput)
			}

			// Verify usage was still extracted for billing
			if extractor.usage == nil || extractor.usage.TotalTokens == 0 {
				t.Errorf("%s: Usage data should still be extracted for billing even when filtered from output", tt.name)
			}
		})
	}
}

func TestExtractTokensFromResponseWithHandler_Parameters(t *testing.T) {
	// Test that the function properly accepts and uses the clientRequestedUsage parameter
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader("data: test\n")),
	}

	usageHandler := func(usage *Usage) {
		// Handler for testing
	}

	// Call with clientRequestedUsage = true
	body, usage, err := ExtractTokensFromResponseWithHandler(resp, "test-model", usageHandler, true)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if body == nil {
		t.Error("Expected non-nil body")
	}
	if usage != nil {
		t.Error("Expected nil usage for streaming response")
	}

	// Test backward compatibility
	body2, usage2, err2 := ExtractTokensFromResponse(resp, "test-model")
	if err2 != nil {
		t.Errorf("Unexpected error in backward compatible function: %v", err2)
	}
	if body2 == nil {
		t.Error("Expected non-nil body from backward compatible function")
	}
	if usage2 != nil {
		t.Error("Expected nil usage for streaming response from backward compatible function")
	}
}

func TestStreamingWithContinuousUsageStats(t *testing.T) {
	// Test case that mimics vLLM's continuous usage stats behavior
	tests := []struct {
		name                 string
		clientRequestedUsage bool
		input                string
		expectedChunks       int
		expectedEmptyChoices int
		description          string
	}{
		{
			name:                 "vllm_continuous_stats_no_client_request",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[{"delta":{"content":"1"},"index":0}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}
data: {"id":"test","choices":[{"delta":{"content":" "},"index":0}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}
data: {"id":"test","choices":[{"delta":{"content":"2"},"index":0}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}
data: {"id":"test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}
data: [DONE]
`,
			expectedChunks:       4, // Last empty choices chunk should be filtered
			expectedEmptyChoices: 0,
			description:          "vLLM continuous stats with final empty choices chunk - should filter when not requested",
		},
		{
			name:                 "vllm_continuous_stats_with_client_request",
			clientRequestedUsage: true,
			input: `data: {"id":"test","choices":[{"delta":{"content":"Hi"},"index":0}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}
data: {"id":"test","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}
data: [DONE]
`,
			expectedChunks:       3, // All chunks including empty choices should pass through
			expectedEmptyChoices: 1,
			description:          "vLLM continuous stats with final empty choices chunk - should pass through when requested",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputReader := io.NopCloser(strings.NewReader(tt.input))
			outputBuffer := &bytes.Buffer{}
			pr, pw := io.Pipe()

			extractor := NewStreamingTokenExtractor(inputReader, pw, "test-model")
			extractor.clientRequestedUsage = tt.clientRequestedUsage

			processingDone := make(chan bool)
			go func() {
				extractor.processStream()
				processingDone <- true
			}()

			readDone := make(chan bool)
			go func() {
				io.Copy(outputBuffer, pr)
				readDone <- true
			}()

			<-processingDone
			<-readDone

			// Count chunks in output
			output := outputBuffer.String()
			lines := strings.Split(output, "\n")
			dataChunks := 0
			emptyChoicesChunks := 0

			for _, line := range lines {
				if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
					dataChunks++
					if strings.Contains(line, `"choices":[]`) {
						emptyChoicesChunks++
					}
				}
			}

			if dataChunks != tt.expectedChunks {
				t.Errorf("%s: Expected %d chunks, got %d", tt.description, tt.expectedChunks, dataChunks)
			}
			if emptyChoicesChunks != tt.expectedEmptyChoices {
				t.Errorf("%s: Expected %d empty choices chunks, got %d", tt.description, tt.expectedEmptyChoices, emptyChoicesChunks)
			}
		})
	}
}

func TestUsageExtraction_NotAffectedByFiltering(t *testing.T) {
	// Ensure that usage extraction for billing works regardless of filtering
	input := `data: {"id":"test","choices":[{"delta":{"content":"Hello"},"index":0}]}
data: {"id":"test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
data: [DONE]
`

	// Test with client not requesting usage (should filter)
	inputReader := io.NopCloser(strings.NewReader(input))
	outputBuffer := &bytes.Buffer{}
	pr, pw := io.Pipe()

	var capturedUsage *Usage
	usageHandlerCalled := make(chan bool, 1)
	extractor := NewStreamingTokenExtractor(inputReader, pw, "test-model")
	extractor.clientRequestedUsage = false
	extractor.usageHandler = func(usage *Usage) {
		capturedUsage = usage
		usageHandlerCalled <- true
	}

	// Start processing
	processingDone := make(chan bool)
	go func() {
		extractor.processStream()
		processingDone <- true
	}()

	// Read output
	readDone := make(chan bool)
	go func() {
		io.Copy(outputBuffer, pr)
		readDone <- true
	}()

	// Wait for processing to complete
	<-processingDone
	<-readDone

	// Wait for usage handler to be called
	select {
	case <-usageHandlerCalled:
		// Handler was called
	case <-time.After(100 * time.Millisecond):
		t.Error("Usage handler should have been called")
		return
	}

	// Verify usage was captured for billing
	if capturedUsage == nil {
		t.Error("Usage handler should have been called with non-nil usage")
	} else {
		if capturedUsage.PromptTokens != 10 {
			t.Errorf("Expected prompt tokens 10, got %d", capturedUsage.PromptTokens)
		}
		if capturedUsage.CompletionTokens != 5 {
			t.Errorf("Expected completion tokens 5, got %d", capturedUsage.CompletionTokens)
		}
		if capturedUsage.TotalTokens != 15 {
			t.Errorf("Expected total tokens 15, got %d", capturedUsage.TotalTokens)
		}
	}

	// Verify the usage chunk was filtered from output
	output := outputBuffer.String()
	if strings.Contains(output, `"usage":`) {
		t.Error("Usage chunk should have been filtered from output when client didn't request it")
	}
}

func TestEdgeCases(t *testing.T) {
	tests := []struct {
		name                 string
		clientRequestedUsage bool
		input                string
		description          string
		expectFiltered       bool
	}{
		{
			name:                 "empty_choices_without_usage",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[]}
data: {"id":"test","choices":[{"delta":{"content":"Hi"},"index":0}]}
data: [DONE]
`,
			description:    "Empty choices without usage data should pass through",
			expectFiltered: false,
		},
		{
			name:                 "null_usage_field",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[],"usage":null}
data: {"id":"test","choices":[{"delta":{"content":"Hi"},"index":0}]}
data: [DONE]
`,
			description:    "Empty choices with null usage should pass through",
			expectFiltered: false,
		},
		{
			name:                 "partial_usage_data",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[],"usage":{"prompt_tokens":10}}
data: {"id":"test","choices":[{"delta":{"content":"Hi"},"index":0}]}
data: [DONE]
`,
			description:    "Empty choices with partial usage data should be filtered",
			expectFiltered: true,
		},
		{
			name:                 "usage_in_finish_chunk",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[{"delta":{"content":"Hi"},"index":0}]}
data: {"id":"test","choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"total_tokens":10}}
data: [DONE]
`,
			description:    "Usage in finish_reason chunk (non-empty choices) should pass through",
			expectFiltered: false,
		},
		{
			name:                 "nested_arrays_in_choices",
			clientRequestedUsage: false,
			input: `data: {"id":"test","choices":[[]],"usage":{"total_tokens":10}}
data: [DONE]
`,
			description:    "Malformed choices (nested array) should pass through unchanged",
			expectFiltered: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputReader := io.NopCloser(strings.NewReader(tt.input))
			outputBuffer := &bytes.Buffer{}
			pr, pw := io.Pipe()

			extractor := NewStreamingTokenExtractor(inputReader, pw, "test-model")
			extractor.clientRequestedUsage = tt.clientRequestedUsage

			processingDone := make(chan bool)
			go func() {
				extractor.processStream()
				processingDone <- true
			}()

			readDone := make(chan bool)
			go func() {
				io.Copy(outputBuffer, pr)
				readDone <- true
			}()

			<-processingDone
			<-readDone

			output := outputBuffer.String()

			// Check if the specific line was filtered
			lines := strings.Split(tt.input, "\n")
			firstLine := lines[0]

			if tt.expectFiltered {
				if strings.Contains(output, firstLine) {
					t.Errorf("%s: Expected first line to be filtered but it was present", tt.description)
				}
			} else {
				if !strings.Contains(output, firstLine) {
					t.Errorf("%s: Expected first line to be present but it was filtered", tt.description)
				}
			}
		})
	}
}

func TestConcurrentStreaming(t *testing.T) {
	// Test that multiple concurrent streams don't interfere with each other
	input1 := `data: {"id":"test1","choices":[{"delta":{"content":"Stream1"},"index":0}]}
data: {"id":"test1","choices":[],"usage":{"total_tokens":10}}
data: [DONE]
`
	input2 := `data: {"id":"test2","choices":[{"delta":{"content":"Stream2"},"index":0}]}
data: {"id":"test2","choices":[],"usage":{"total_tokens":20}}
data: [DONE]
`

	// Run two extractors concurrently
	output1 := &bytes.Buffer{}
	output2 := &bytes.Buffer{}

	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()

	extractor1 := NewStreamingTokenExtractor(io.NopCloser(strings.NewReader(input1)), pw1, "model1")
	extractor1.clientRequestedUsage = false // Should filter

	extractor2 := NewStreamingTokenExtractor(io.NopCloser(strings.NewReader(input2)), pw2, "model2")
	extractor2.clientRequestedUsage = true // Should NOT filter

	done := make(chan bool, 4)

	// Process both streams
	go func() {
		extractor1.processStream()
		done <- true
	}()
	go func() {
		extractor2.processStream()
		done <- true
	}()

	// Read outputs
	go func() {
		io.Copy(output1, pr1)
		done <- true
	}()
	go func() {
		io.Copy(output2, pr2)
		done <- true
	}()

	// Wait for all to complete
	for i := 0; i < 4; i++ {
		<-done
	}

	// Verify stream 1 filtered the usage chunk
	if strings.Contains(output1.String(), `"choices":[]`) {
		t.Error("Stream 1 should have filtered empty choices chunk")
	}

	// Verify stream 2 did NOT filter the usage chunk
	if !strings.Contains(output2.String(), `"choices":[]`) {
		t.Error("Stream 2 should NOT have filtered empty choices chunk")
	}
}

func TestRealWorldScenarios(t *testing.T) {
	// Test scenarios based on actual vLLM and OpenAI responses
	tests := []struct {
		name                   string
		clientRequestedUsage   bool
		scenario               string
		input                  string
		shouldHaveEmptyChoices bool
	}{
		{
			name:                 "openai_style_streaming_without_usage",
			clientRequestedUsage: false,
			scenario:             "Standard OpenAI streaming (no usage in stream)",
			input: `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`,
			shouldHaveEmptyChoices: false,
		},
		{
			name:                 "openai_style_streaming_with_usage",
			clientRequestedUsage: true,
			scenario:             "OpenAI streaming with include_usage=true",
			input: `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}],"usage":null}
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}
data: [DONE]
`,
			shouldHaveEmptyChoices: true,
		},
		{
			name:                 "vllm_continuous_usage_stats",
			clientRequestedUsage: false,
			scenario:             "vLLM with continuous_usage_stats=true (always added by proxy)",
			input: `data: {"id":"cmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"llama","choices":[{"index":0,"delta":{"content":"Test"},"finish_reason":null}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}
data: {"id":"cmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"llama","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}
data: {"id":"cmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"llama","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}
data: [DONE]
`,
			shouldHaveEmptyChoices: false, // Should be filtered since client didn't request
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputReader := io.NopCloser(strings.NewReader(tt.input))
			outputBuffer := &bytes.Buffer{}
			pr, pw := io.Pipe()

			extractor := NewStreamingTokenExtractor(inputReader, pw, "test-model")
			extractor.clientRequestedUsage = tt.clientRequestedUsage

			processingDone := make(chan bool)
			go func() {
				extractor.processStream()
				processingDone <- true
			}()

			readDone := make(chan bool)
			go func() {
				io.Copy(outputBuffer, pr)
				readDone <- true
			}()

			<-processingDone
			<-readDone

			output := outputBuffer.String()
			hasEmptyChoices := strings.Contains(output, `"choices":[]`)

			if hasEmptyChoices != tt.shouldHaveEmptyChoices {
				t.Errorf("%s: Expected empty choices=%v, got %v\nScenario: %s",
					tt.name, tt.shouldHaveEmptyChoices, hasEmptyChoices, tt.scenario)
			}
		})
	}
}
