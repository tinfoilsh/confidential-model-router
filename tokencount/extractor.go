package tokencount

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

// APIType represents the type of API being used
type APIType string

const (
	APITypeCompletions APIType = "completions"
	APITypeResponses   APIType = "responses"
)

// Usage represents token usage information from inference responses
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIResponse represents a standard OpenAI-compatible response
type OpenAIResponse struct {
	Usage *Usage `json:"usage,omitempty"`
}

// JSONTokenExtractor accumulates JSON response data and extracts tokens
type JSONTokenExtractor struct {
	buffer  bytes.Buffer
	model   string
	usage   *Usage
	mu      sync.Mutex
	apiType APIType
}

// Write implements io.Writer, accumulating data
func (j *JSONTokenExtractor) Write(p []byte) (n int, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buffer.Write(p)
}

// ExtractUsage parses the accumulated JSON and extracts token usage
func (j *JSONTokenExtractor) ExtractUsage() {
	j.mu.Lock()
	defer j.mu.Unlock()

	data := j.buffer.Bytes()

	switch j.apiType {
	case APITypeResponses:
		j.extractResponsesAPIUsage(data)
	default:
		j.extractChatCompletionsUsage(data)
	}

	if j.usage != nil {
		log.Printf("[TokenExtractor] Model: %s, Tokens - Input: %d, Output: %d, Total: %d",
			j.model,
			j.usage.PromptTokens,
			j.usage.CompletionTokens,
			j.usage.TotalTokens,
		)
	}
}

func (j *JSONTokenExtractor) extractChatCompletionsUsage(data []byte) {
	var resp OpenAIResponse
	if err := json.Unmarshal(data, &resp); err == nil && resp.Usage != nil {
		j.usage = resp.Usage
	}
}

func (j *JSONTokenExtractor) extractResponsesAPIUsage(data []byte) {
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		return
	}

	usageData, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}

	j.usage = &Usage{}
	if inputTokens, ok := usageData["input_tokens"].(float64); ok {
		j.usage.PromptTokens = int(inputTokens)
	}
	if outputTokens, ok := usageData["output_tokens"].(float64); ok {
		j.usage.CompletionTokens = int(outputTokens)
	}
	if totalTokens, ok := usageData["total_tokens"].(float64); ok {
		j.usage.TotalTokens = int(totalTokens)
	} else {
		j.usage.TotalTokens = j.usage.PromptTokens + j.usage.CompletionTokens
	}
}

// teeReaderCloser wraps a TeeReader and extracts tokens on close
type teeReaderCloser struct {
	io.Reader
	origBody     io.ReadCloser
	extractor    *JSONTokenExtractor
	usageHandler func(*Usage)
}

// Close extracts tokens and closes the original body
func (t *teeReaderCloser) Close() error {
	// Extract usage from accumulated data
	t.extractor.ExtractUsage()

	// Call usage handler if provided
	if t.usageHandler != nil && t.extractor.usage != nil {
		t.usageHandler(t.extractor.usage)
	}

	// Close original body
	return t.origBody.Close()
}

// ExtractTokensFromResponse extracts token counts from HTTP response using TeeReader
// It doesn't buffer the response, allowing streaming to work properly
func ExtractTokensFromResponse(resp *http.Response, model string) (io.ReadCloser, *Usage, error) {
	return ExtractTokensFromResponseWithHandler(resp, model, nil, false, APITypeCompletions)
}

// ExtractTokensFromResponseWithHandler extracts token counts with an optional usage handler for streaming
// clientRequestedUsage indicates if the client explicitly requested usage stats in their request
func ExtractTokensFromResponseWithHandler(resp *http.Response, model string, usageHandler func(*Usage), clientRequestedUsage bool, apiType APIType) (io.ReadCloser, *Usage, error) {
	contentType := resp.Header.Get("Content-Type")

	// For streaming responses, use the streaming extractor
	if strings.Contains(contentType, "text/event-stream") {
		pr, pw := io.Pipe()
		extractor := NewStreamingTokenExtractor(resp.Body, pw, model)
		extractor.usageHandler = usageHandler
		extractor.clientRequestedUsage = clientRequestedUsage
		extractor.apiType = apiType
		go extractor.processStream()
		return pr, nil, nil
	}

	// For non-JSON or non-200 responses, pass through unchanged
	if resp.StatusCode != http.StatusOK || !strings.Contains(contentType, "application/json") {
		return resp.Body, nil, nil
	}

	// For JSON responses, use TeeReader to avoid buffering
	extractor := &JSONTokenExtractor{
		model:   model,
		apiType: apiType,
	}

	// TeeReader copies data to extractor while passing it through
	teeReader := io.TeeReader(resp.Body, extractor)

	// Return a custom closer that logs tokens when closed
	return &teeReaderCloser{
		Reader:       teeReader,
		origBody:     resp.Body,
		extractor:    extractor,
		usageHandler: usageHandler,
	}, nil, nil
}

// StreamingTokenExtractor handles token extraction for streaming responses
type StreamingTokenExtractor struct {
	reader               io.ReadCloser
	writer               io.WriteCloser
	model                string
	usage                *Usage
	buffer               bytes.Buffer
	scanner              *bufio.Scanner
	completed            bool
	usageHandler         func(*Usage) // Callback for when usage is extracted
	clientRequestedUsage bool         // Whether client explicitly requested usage stats
	apiType              APIType      // The API type (e.g., completions or responses)
}

// NewStreamingTokenExtractor creates a new streaming token extractor that intercepts SSE chunks
func NewStreamingTokenExtractor(reader io.ReadCloser, writer io.WriteCloser, model string) *StreamingTokenExtractor {
	s := &StreamingTokenExtractor{
		reader: reader,
		writer: writer,
		model:  model,
		usage:  &Usage{},
	}
	s.scanner = bufio.NewScanner(reader)
	return s
}

// processStream processes the SSE stream, extracting token usage
func (s *StreamingTokenExtractor) processStream() {
	defer s.writer.Close()

	switch s.apiType {
	case APITypeResponses:
		s.processResponsesAPIStream()
	default:
		s.processChatCompletionsStream()
	}

	// Log final usage if we collected any
	if s.usage != nil && (s.usage.TotalTokens > 0 || s.usage.CompletionTokens > 0) {
		// If we only have completion tokens, estimate total
		if s.usage.TotalTokens == 0 && s.usage.CompletionTokens > 0 {
			s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
		}

		log.Printf("[StreamingTokenExtractor] Model: %s, Tokens - Input: %d, Output: %d, Total: %d",
			s.model,
			s.usage.PromptTokens,
			s.usage.CompletionTokens,
			s.usage.TotalTokens,
		)

		// Call usage handler if provided
		if s.usageHandler != nil {
			s.usageHandler(s.usage)
		}
	}

	s.completed = true
}

// processChatCompletionsStream handles the Chat Completions API streaming format
func (s *StreamingTokenExtractor) processChatCompletionsStream() {
	lastLineWasFiltered := false

	for s.scanner.Scan() {
		line := s.scanner.Text()
		shouldWrite := true

		// If the previous line was filtered and this is an empty line, skip it
		// to avoid consecutive empty lines in the output
		if lastLineWasFiltered && line == "" {
			lastLineWasFiltered = false
			continue
		}

		lastLineWasFiltered = false

		// Parse SSE data lines
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data != "[DONE]" {
				// Try to parse the chunk
				var chunk map[string]any
				if err := json.Unmarshal([]byte(data), &chunk); err == nil {
					// Check for usage in the chunk
					if usageData, ok := chunk["usage"]; ok && usageData != nil {
						usageBytes, _ := json.Marshal(usageData)
						var usage Usage
						if err := json.Unmarshal(usageBytes, &usage); err == nil {
							// Update usage data (continuous stats may send incremental updates)
							if usage.PromptTokens > 0 {
								s.usage.PromptTokens = usage.PromptTokens
							}
							if usage.CompletionTokens > 0 {
								s.usage.CompletionTokens = usage.CompletionTokens
							}
							if usage.TotalTokens > 0 {
								s.usage.TotalTokens = usage.TotalTokens
							}
						}

						// Check if this is a usage-only chunk that should be filtered
						if !s.clientRequestedUsage {
							// Check if choices array exists and is empty
							if choices, hasChoices := chunk["choices"].([]any); hasChoices && len(choices) == 0 {
								// This is a usage-only chunk with empty choices array
								// Filter it out since client didn't request usage
								shouldWrite = false
								lastLineWasFiltered = true
							}
						}
					}
				}
			}
		}

		// Write the line to output if we should
		if shouldWrite {
			if line == "" {
				s.writer.Write([]byte("\n"))
			} else {
				s.writer.Write([]byte(line + "\n"))
			}
		}
	}

	if err := s.scanner.Err(); err != nil {
		log.Printf("[StreamingTokenExtractor] Scanner error: %v", err)
	}
}

// processResponsesAPIStream handles the OpenAI Responses API streaming format
// Events have the format:
//
//	event: response.output_text.delta
//	data: {"type":"response.output_text.delta","delta":"Hello",...}
//
// Usage comes in response.completed events.
// See: https://platform.openai.com/docs/api-reference/responses-streaming
func (s *StreamingTokenExtractor) processResponsesAPIStream() {
	lastLineWasFiltered := false

	for s.scanner.Scan() {
		line := s.scanner.Text()
		shouldWrite := true

		// If the previous line was filtered and this is an empty line, skip it
		// to avoid consecutive empty lines in the output
		if lastLineWasFiltered && line == "" {
			lastLineWasFiltered = false
			continue
		}

		lastLineWasFiltered = false

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]any
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				eventType, _ := event["type"].(string)

				// Extract usage from response.completed events
				// See: https://platform.openai.com/docs/api-reference/responses-streaming
				if eventType == "response.completed" {
					if response, ok := event["response"].(map[string]any); ok {
						s.extractResponsesAPIUsage(response)

						// Filter out usage from the response if client didn't request it
						if !s.clientRequestedUsage {
							if _, hasUsage := response["usage"]; hasUsage {
								delete(response, "usage")
								event["response"] = response
								// Re-encode the modified event
								if modifiedData, err := json.Marshal(event); err == nil {
									line = "data: " + string(modifiedData)
								}
							}
						}
					} else {
						s.extractResponsesAPIUsage(event)
					}
				}
			}
		}

		// Write the line to output if we should
		if shouldWrite {
			if line == "" {
				s.writer.Write([]byte("\n"))
			} else {
				s.writer.Write([]byte(line + "\n"))
			}
		}
	}

	if err := s.scanner.Err(); err != nil {
		log.Printf("[StreamingTokenExtractor] Scanner error: %v", err)
	}
}

// extractResponsesAPIUsage extracts usage from a Responses API response object
func (s *StreamingTokenExtractor) extractResponsesAPIUsage(data map[string]any) {
	usageData, ok := data["usage"].(map[string]any)
	if !ok {
		return
	}

	if s.usage == nil {
		s.usage = &Usage{}
	}

	if inputTokens, ok := usageData["input_tokens"].(float64); ok {
		s.usage.PromptTokens = int(inputTokens)
	}
	if outputTokens, ok := usageData["output_tokens"].(float64); ok {
		s.usage.CompletionTokens = int(outputTokens)
	}
	if totalTokens, ok := usageData["total_tokens"].(float64); ok {
		s.usage.TotalTokens = int(totalTokens)
	} else {
		s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
	}
}

// Read implements io.Reader for compatibility
func (s *StreamingTokenExtractor) Read(p []byte) (n int, err error) {
	// This is mainly for compatibility - actual processing happens in processStream
	return s.buffer.Read(p)
}

// Close implements io.Closer
func (s *StreamingTokenExtractor) Close() error {
	return s.reader.Close()
}
