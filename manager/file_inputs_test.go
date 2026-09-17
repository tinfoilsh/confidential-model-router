package manager

import (
	"errors"
	"net/http"
	"strings"
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

func TestParseDocUploadResponseImagesMode(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		expectedMD    string
		expectedPages int
		expectedError string
	}{
		{
			name:          "valid_with_text_and_pages",
			body:          `{"document":{"md_content":"page text","pages":[{"page":1,"image":"data:image/png;base64,AAA","is_scanned":false}]}}`,
			expectedMD:    "page text",
			expectedPages: 1,
		},
		{
			name:          "valid_with_pages_only",
			body:          `{"document":{"md_content":"","pages":[{"page":1,"image":"data:image/png;base64,AAA","is_scanned":true}]}}`,
			expectedMD:    "",
			expectedPages: 1,
		},
		{
			name:          "missing_pages_in_images_mode",
			body:          `{"document":{"md_content":"only text"}}`,
			expectedError: errMsgDocumentNoPages,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDocUploadResponse([]byte(tt.body), FileConversionModeImages)
			if tt.expectedError != "" {
				if err == nil || err.Error() != tt.expectedError {
					t.Fatalf("expected %q, got %v", tt.expectedError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.MDContent != tt.expectedMD {
				t.Fatalf("MDContent=%q, want %q", got.MDContent, tt.expectedMD)
			}
			if len(got.Pages) != tt.expectedPages {
				t.Fatalf("len(Pages)=%d, want %d", len(got.Pages), tt.expectedPages)
			}
		})
	}
}

func TestFileConversionModeIsValid(t *testing.T) {
	for _, mode := range []FileConversionMode{
		"",
		FileConversionModeText,
		FileConversionModeVision,
		FileConversionModeImages,
		FileConversionModeRaw,
		FileConversionModeVLM,
	} {
		if !mode.IsValid() {
			t.Errorf("expected %q to be a valid mode", mode)
		}
	}
	for _, mode := range []FileConversionMode{
		"text-mode",
		"AUTO",
		"unknown",
	} {
		if mode.IsValid() {
			t.Errorf("expected %q to be invalid", mode)
		}
	}
}

func TestParseDocUploadResponseTextMode(t *testing.T) {
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
			expectedError: errMsgDocumentInvalidResponse,
			expectedCode:  http.StatusBadGateway,
		},
		{
			name:          "empty_markdown",
			body:          `{"document":{"md_content":""}}`,
			expectedError: errMsgDocumentEmpty,
			expectedCode:  http.StatusBadGateway,
		},
		{
			name:          "whitespace_only_markdown",
			body:          `{"document":{"md_content":" \n\t "}}`,
			expectedError: errMsgDocumentEmpty,
			expectedCode:  http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDocUploadResponse([]byte(tt.body), FileConversionModeText)
			if tt.expectedError == "" {
				if err != nil {
					t.Fatalf("parseDocUploadResponse returned unexpected error: %v", err)
				}
				if got.MDContent != tt.expected {
					t.Fatalf("MDContent = %q, want %q", got.MDContent, tt.expected)
				}
				return
			}

			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tt.expectedError {
				t.Fatalf("unexpected error: %v", err)
			}

			var conversionErr *APIError
			if !errors.As(err, &conversionErr) {
				t.Fatalf("expected APIError, got %T", err)
			}
			if conversionErr.Status != tt.expectedCode {
				t.Fatalf("expected status %d, got %d", tt.expectedCode, conversionErr.Status)
			}
		})
	}
}

func TestUpstreamDocumentErrorBoundsAndClassifiesEnclaveBody(t *testing.T) {
	long := strings.Repeat("x", maxDocumentErrorDetailBytes+100)
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantType   string
		wantMsg    string
	}{
		{"client fault keeps status and body", http.StatusUnprocessableEntity, "unsupported file type\n", http.StatusUnprocessableEntity, ErrTypeInvalidRequest, "Document processing failed: unsupported file type"},
		{"enclave 5xx becomes 502", http.StatusInternalServerError, "boom", http.StatusBadGateway, ErrTypeServer, "Document processing failed: boom"},
		{"empty body uses fixed message", http.StatusBadRequest, "  ", http.StatusBadRequest, ErrTypeInvalidRequest, errMsgDocumentFailed},
		{"oversized body is truncated", http.StatusBadRequest, long, http.StatusBadRequest, ErrTypeInvalidRequest, "Document processing failed: " + long[:maxDocumentErrorDetailBytes] + "..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := upstreamDocumentError(tc.status, []byte(tc.body))
			if got.Status != tc.wantStatus || got.Type != tc.wantType || got.Code != ErrCodeDocumentProcessing {
				t.Fatalf("status/type/code = %d/%s/%s", got.Status, got.Type, got.Code)
			}
			if got.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", got.Message, tc.wantMsg)
			}
		})
	}
}
