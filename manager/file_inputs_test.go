package manager

import (
	"errors"
	"net/http"
	"testing"
)

func TestEscapeMultipartFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain",
			input:    "report.pdf",
			expected: "report.pdf",
		},
		{
			name:     "quotes and backslashes",
			input:    `folder\"quoted".pdf`,
			expected: `folder\\\"quoted\".pdf`,
		},
		{
			name:     "crlf injection",
			input:    "evil\r\nX-Injected: yes\r\n\r\nbody.pdf",
			expected: "evil__X-Injected: yes____body.pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeMultipartFilename(tt.input); got != tt.expected {
				t.Fatalf("escapeMultipartFilename(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSanitizeMultipartContentType(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty",
			input:    "",
			expected: "",
		},
		{
			name:     "valid_with_charset",
			input:    "application/json;charset=utf-8",
			expected: "application/json; charset=utf-8",
		},
		{
			name:     "invalid",
			input:    "application/json; charset",
			expected: "",
		},
		{
			name:     "crlf_injection",
			input:    "application/pdf\r\nX-Injected: yes",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeMultipartContentType(tt.input); got != tt.expected {
				t.Fatalf("sanitizeMultipartContentType(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestParseDocUploadMarkdown(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		expected      string
		expectedError string
		expectedCode  int
	}{
		{
			name:     "valid_markdown",
			body:     `{"document":{"md_content":"# heading\nbody"}}`,
			expected: "# heading\nbody",
		},
		{
			name:          "invalid_json",
			body:          `{`,
			expectedError: "invalid document processing response",
			expectedCode:  http.StatusBadGateway,
		},
		{
			name:          "empty_markdown",
			body:          `{"document":{"md_content":""}}`,
			expectedError: "document processing returned empty content",
			expectedCode:  http.StatusBadGateway,
		},
		{
			name:          "whitespace_only_markdown",
			body:          `{"document":{"md_content":" \n\t "}}`,
			expectedError: "document processing returned empty content",
			expectedCode:  http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDocUploadMarkdown([]byte(tt.body))
			if tt.expectedError == "" {
				if err != nil {
					t.Fatalf("parseDocUploadMarkdown returned unexpected error: %v", err)
				}
				if got != tt.expected {
					t.Fatalf("parseDocUploadMarkdown() = %q, want %q", got, tt.expected)
				}
				return
			}

			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tt.expectedError {
				t.Fatalf("unexpected error: %v", err)
			}

			var conversionErr *FileConversionError
			if !errors.As(err, &conversionErr) {
				t.Fatalf("expected FileConversionError, got %T", err)
			}
			if conversionErr.StatusCode != tt.expectedCode {
				t.Fatalf("expected status %d, got %d", tt.expectedCode, conversionErr.StatusCode)
			}
		})
	}
}
