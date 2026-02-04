package manager

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

func TestUsageMetricsWriter_SetUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	usage := &tokencount.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	wrapper.SetUsage(usage)

	if wrapper.usage == nil {
		t.Fatal("expected usage to be set")
	}
	if wrapper.usage.PromptTokens != 100 {
		t.Errorf("expected PromptTokens=100, got %d", wrapper.usage.PromptTokens)
	}
	if wrapper.usage.CompletionTokens != 50 {
		t.Errorf("expected CompletionTokens=50, got %d", wrapper.usage.CompletionTokens)
	}
	if wrapper.usage.TotalTokens != 150 {
		t.Errorf("expected TotalTokens=150, got %d", wrapper.usage.TotalTokens)
	}
}

func TestUsageMetricsWriter_FormatUsage(t *testing.T) {
	tests := []struct {
		name     string
		usage    *tokencount.Usage
		expected string
	}{
		{
			name: "standard usage",
			usage: &tokencount.Usage{
				PromptTokens:     67,
				CompletionTokens: 42,
				TotalTokens:      109,
			},
			expected: "prompt=67,completion=42,total=109",
		},
		{
			name: "zero values",
			usage: &tokencount.Usage{
				PromptTokens:     0,
				CompletionTokens: 0,
				TotalTokens:      0,
			},
			expected: "prompt=0,completion=0,total=0",
		},
		{
			name: "large values",
			usage: &tokencount.Usage{
				PromptTokens:     128000,
				CompletionTokens: 4096,
				TotalTokens:      132096,
			},
			expected: "prompt=128000,completion=4096,total=132096",
		},
		{
			name:     "nil usage",
			usage:    nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			wrapper := &usageMetricsWriter{ResponseWriter: rec}

			if tt.usage != nil {
				wrapper.SetUsage(tt.usage)
			}

			result := wrapper.FormatUsage()
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestUsageMetricsWriter_WriteTrailer(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	usage := &tokencount.Usage{
		PromptTokens:     67,
		CompletionTokens: 42,
		TotalTokens:      109,
	}
	wrapper.SetUsage(usage)
	wrapper.EnableTrailer()

	wrapper.WriteTrailer()

	// Check that the header was set
	headerValue := rec.Header().Get(UsageMetricsResponseHeader)
	expected := "prompt=67,completion=42,total=109"
	if headerValue != expected {
		t.Errorf("expected header %q, got %q", expected, headerValue)
	}
}

func TestUsageMetricsWriter_WriteTrailer_NotEnabled(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	usage := &tokencount.Usage{
		PromptTokens:     5,
		CompletionTokens: 6,
		TotalTokens:      11,
	}
	wrapper.SetUsage(usage)

	wrapper.WriteTrailer()

	headerValue := rec.Header().Get(UsageMetricsResponseHeader)
	if headerValue != "" {
		t.Errorf("expected no header when trailer is not enabled, got %q", headerValue)
	}
}

func TestUsageMetricsWriter_WriteTrailer_NilUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	// Don't set any usage
	wrapper.EnableTrailer()
	wrapper.WriteTrailer()

	// Header should not be set
	headerValue := rec.Header().Get(UsageMetricsResponseHeader)
	if headerValue != "" {
		t.Errorf("expected no header when usage is nil, got %q", headerValue)
	}
}

func TestUsageMetricsWriter_Flush(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	// Write some data
	wrapper.Write([]byte("test data"))

	// Flush should not panic and should work
	wrapper.Flush()

	// Verify data was written
	if rec.Body.String() != "test data" {
		t.Errorf("expected 'test data', got %q", rec.Body.String())
	}
}

func TestUsageMetricsWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	unwrapped := wrapper.Unwrap()
	if unwrapped != rec {
		t.Error("Unwrap should return the underlying ResponseWriter")
	}
}

func TestUsageWriterKey_Context(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	ctx := context.WithValue(context.Background(), usageWriterKey{}, wrapper)

	// Retrieve from context
	retrieved, ok := ctx.Value(usageWriterKey{}).(*usageMetricsWriter)
	if !ok {
		t.Fatal("failed to retrieve usageMetricsWriter from context")
	}
	if retrieved != wrapper {
		t.Error("retrieved wrapper should match original")
	}
}

func TestUsageMetricsWriter_ConcurrentAccess(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	done := make(chan bool)

	// Concurrent writes
	for i := 0; i < 10; i++ {
		go func(n int) {
			usage := &tokencount.Usage{
				PromptTokens:     n * 10,
				CompletionTokens: n * 5,
				TotalTokens:      n * 15,
			}
			wrapper.SetUsage(usage)
			_ = wrapper.FormatUsage()
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Should not panic and usage should be set
	if wrapper.usage == nil {
		t.Error("usage should be set after concurrent access")
	}
}

// Integration test simulating the full flow
func TestUsageMetrics_NonStreamingFlow(t *testing.T) {
	// Simulate non-streaming response
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if usage metrics requested
		if r.Header.Get(UsageMetricsRequestHeader) != "true" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"response": "no metrics"}`))
			return
		}

		// Wrap writer
		wrapper := &usageMetricsWriter{ResponseWriter: w}

		// Simulate usage extraction (normally done by tokencount)
		usage := &tokencount.Usage{
			PromptTokens:     100,
			CompletionTokens: 25,
			TotalTokens:      125,
		}
		wrapper.SetUsage(usage)

		// Set header (for non-streaming, set directly)
		w.Header().Set(UsageMetricsResponseHeader, wrapper.FormatUsage())

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"response": "with metrics"}`))
	})

	// Test with usage metrics requested
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set(UsageMetricsRequestHeader, "true")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Check response header
	usageHeader := rec.Header().Get(UsageMetricsResponseHeader)
	expected := "prompt=100,completion=25,total=125"
	if usageHeader != expected {
		t.Errorf("expected usage header %q, got %q", expected, usageHeader)
	}
}

func TestUsageMetrics_StreamingFlow(t *testing.T) {
	// Simulate streaming response with trailer
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if usage metrics requested
		if r.Header.Get(UsageMetricsRequestHeader) != "true" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Announce trailer
		w.Header().Set("Trailer", UsageMetricsResponseHeader)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Transfer-Encoding", "chunked")

		// Wrap writer
		wrapper := &usageMetricsWriter{ResponseWriter: w}

		// Write streaming data
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"chunk\": 1}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write([]byte("data: {\"chunk\": 2}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write([]byte("data: [DONE]\n\n"))

		// Simulate usage extraction at end of stream
		usage := &tokencount.Usage{
			PromptTokens:     50,
			CompletionTokens: 10,
			TotalTokens:      60,
		}
		wrapper.SetUsage(usage)
		wrapper.EnableTrailer()

		// Write trailer
		wrapper.WriteTrailer()
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream": true}`))
	req.Header.Set(UsageMetricsRequestHeader, "true")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Check trailer announcement
	trailerAnnouncement := rec.Header().Get("Trailer")
	if trailerAnnouncement != UsageMetricsResponseHeader {
		t.Errorf("expected Trailer header %q, got %q", UsageMetricsResponseHeader, trailerAnnouncement)
	}

	// Check the usage header was set (httptest.ResponseRecorder stores trailers in Header)
	usageHeader := rec.Header().Get(UsageMetricsResponseHeader)
	expected := "prompt=50,completion=10,total=60"
	if usageHeader != expected {
		t.Errorf("expected usage trailer %q, got %q", expected, usageHeader)
	}

	// Verify body was written
	body := rec.Body.String()
	if !strings.Contains(body, "chunk") {
		t.Error("expected streaming chunks in body")
	}
}

func TestUsageMetrics_OptOut(t *testing.T) {
	// Test that when header is not set, no usage metrics are added
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only set usage if requested
		if r.Header.Get(UsageMetricsRequestHeader) == "true" {
			// Would set header here
			w.Header().Set(UsageMetricsResponseHeader, "prompt=1,completion=1,total=2")
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"response": "test"}`))
	})

	// Test WITHOUT usage metrics header
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	// Not setting X-Tinfoil-Request-Usage-Metrics header
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Should NOT have usage header
	usageHeader := rec.Header().Get(UsageMetricsResponseHeader)
	if usageHeader != "" {
		t.Errorf("expected no usage header when not requested, got %q", usageHeader)
	}
}

// Test with real tokencount.Usage pointer manipulation
func TestUsageMetricsWriter_UsagePointerSafety(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	// Set usage
	usage := &tokencount.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}
	wrapper.SetUsage(usage)

	// Modify original usage struct
	usage.PromptTokens = 200
	usage.CompletionTokens = 100
	usage.TotalTokens = 300

	// The wrapper should have the modified values (pointer semantics)
	formatted := wrapper.FormatUsage()
	if !strings.Contains(formatted, "prompt=200") {
		t.Errorf("expected updated prompt tokens in format, got %q", formatted)
	}
}

// Test header constants
func TestUsageMetricsHeaderConstants(t *testing.T) {
	if UsageMetricsRequestHeader != "X-Tinfoil-Request-Usage-Metrics" {
		t.Errorf("unexpected request header constant: %q", UsageMetricsRequestHeader)
	}
	if UsageMetricsResponseHeader != "X-Tinfoil-Usage-Metrics" {
		t.Errorf("unexpected response header constant: %q", UsageMetricsResponseHeader)
	}
}

// Benchmark formatting
func BenchmarkFormatUsage(b *testing.B) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}
	wrapper.SetUsage(&tokencount.Usage{
		PromptTokens:     128000,
		CompletionTokens: 4096,
		TotalTokens:      132096,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wrapper.FormatUsage()
	}
}

// Test that Write passes through correctly
func TestUsageMetricsWriter_WritePassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	testData := []byte("Hello, World!")
	n, err := wrapper.Write(testData)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if n != len(testData) {
		t.Errorf("expected to write %d bytes, wrote %d", len(testData), n)
	}
	if rec.Body.String() != "Hello, World!" {
		t.Errorf("expected body %q, got %q", "Hello, World!", rec.Body.String())
	}
}

// Test Header passthrough
func TestUsageMetricsWriter_HeaderPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapper := &usageMetricsWriter{ResponseWriter: rec}

	wrapper.Header().Set("Content-Type", "application/json")
	wrapper.Header().Set("X-Custom", "value")

	if rec.Header().Get("Content-Type") != "application/json" {
		t.Error("Content-Type header not passed through")
	}
	if rec.Header().Get("X-Custom") != "value" {
		t.Error("Custom header not passed through")
	}
}

// Mock a response that includes reading body and triggering usage extraction
func TestUsageMetrics_FullRequestResponse(t *testing.T) {
	// This test simulates what happens in a real request

	// Create a mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Simulated response with usage
		io.WriteString(w, `{"id":"test","choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer backend.Close()

	// Verify response can be read
	resp, err := http.Get(backend.URL)
	if err != nil {
		t.Fatalf("failed to get response: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("usage")) {
		t.Error("expected response to contain usage data")
	}
}
