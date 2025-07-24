package tokencount

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
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

// ExtractTokensFromResponse extracts token counts from HTTP response
// It reads the response body, extracts tokens, and returns a new body reader
func ExtractTokensFromResponse(resp *http.Response, model string) (io.ReadCloser, *Usage, error) {
	// Read the entire response body
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil, err
	}
	resp.Body.Close()

	// Create new body reader for the response
	newBody := io.NopCloser(bytes.NewReader(bodyBytes))

	// Only try to extract tokens from successful JSON responses
	contentType := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.Contains(contentType, "application/json") {
		return newBody, nil, nil
	}

	// Try to parse as OpenAI response
	var openAIResp OpenAIResponse
	if err := json.Unmarshal(bodyBytes, &openAIResp); err != nil {
		// Not a JSON response or different format, that's OK
		return newBody, nil, nil
	}

	// Extract usage if present
	if openAIResp.Usage != nil {
		log.Printf("[TokenExtractor] Model: %s, Tokens - Input: %d, Output: %d, Total: %d",
			model,
			openAIResp.Usage.PromptTokens,
			openAIResp.Usage.CompletionTokens,
			openAIResp.Usage.TotalTokens,
		)
		return newBody, openAIResp.Usage, nil
	}

	return newBody, nil, nil
}

// StreamingTokenExtractor handles token extraction for streaming responses
type StreamingTokenExtractor struct {
	reader io.ReadCloser
	model  string
	usage  *Usage
}

// NewStreamingTokenExtractor creates a new streaming token extractor
func NewStreamingTokenExtractor(reader io.ReadCloser, model string) *StreamingTokenExtractor {
	return &StreamingTokenExtractor{
		reader: reader,
		model:  model,
		usage:  &Usage{},
	}
}

// Read implements io.Reader, extracting tokens from SSE chunks
func (s *StreamingTokenExtractor) Read(p []byte) (n int, err error) {
	n, err = s.reader.Read(p)
	
	// TODO: Parse SSE chunks and extract token usage from streaming responses
	// For now, we'll just pass through the data
	
	return n, err
}

// Close implements io.Closer
func (s *StreamingTokenExtractor) Close() error {
	if s.usage.TotalTokens > 0 {
		log.Printf("[StreamingTokenExtractor] Model: %s, Final Tokens - Input: %d, Output: %d, Total: %d",
			s.model,
			s.usage.PromptTokens,
			s.usage.CompletionTokens,
			s.usage.TotalTokens,
		)
	}
	return s.reader.Close()
}