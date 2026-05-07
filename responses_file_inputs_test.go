package main

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

type rewriteRunner func(context.Context, map[string]any, string, *rewriteContext) (bool, error)

type rewriteTestCase struct {
	name                string
	run                 rewriteRunner
	buildBody           func(parts ...any) map[string]any
	buildFileDataPart   func(filename string, fileData string) any
	buildFileIDPart     func(fileID string) any
	buildTextPart       func(text string) any
	contentParts        func(body map[string]any) []any
	expectedTextType    string
	expectedImageType   string
	expectedFileIDError string
	expectedFieldName   string
}

func responsesImagePartURL(part any) string {
	m, _ := part.(map[string]any)
	url, _ := m["image_url"].(string)
	return url
}

func chatImagePartURL(part any) string {
	m, _ := part.(map[string]any)
	inner, _ := m["image_url"].(map[string]any)
	url, _ := inner["url"].(string)
	return url
}

var rewriteTestCases = []rewriteTestCase{
	{
		name: "responses",
		run:  rewriteResponsesBase64FilesWithProcessor,
		buildBody: func(parts ...any) map[string]any {
			return map[string]any{
				"model": "gpt-oss-120b",
				"input": []any{
					map[string]any{
						"role":    "user",
						"content": parts,
					},
				},
			}
		},
		buildFileDataPart: func(filename string, fileData string) any {
			return map[string]any{
				"type":      "input_file",
				"filename":  filename,
				"file_data": fileData,
			}
		},
		buildFileIDPart: func(fileID string) any {
			return map[string]any{
				"type":    "input_file",
				"file_id": fileID,
			}
		},
		buildTextPart: func(text string) any {
			return map[string]any{
				"type": "input_text",
				"text": text,
			}
		},
		contentParts: func(body map[string]any) []any {
			return body["input"].([]any)[0].(map[string]any)["content"].([]any)
		},
		expectedTextType:    "input_text",
		expectedImageType:   "input_image",
		expectedFileIDError: "Only input_file.file_data is supported on this endpoint.",
		expectedFieldName:   "input_file.file_data",
	},
	{
		name: "chat_completions",
		run:  rewriteChatCompletionsBase64FilesWithProcessor,
		buildBody: func(parts ...any) map[string]any {
			return map[string]any{
				"model": "gpt-oss-120b",
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": parts,
					},
				},
			}
		},
		buildFileDataPart: func(filename string, fileData string) any {
			return map[string]any{
				"type": "file",
				"file": map[string]any{
					"filename":  filename,
					"file_data": fileData,
				},
			}
		},
		buildFileIDPart: func(fileID string) any {
			return map[string]any{
				"type": "file",
				"file": map[string]any{
					"file_id": fileID,
				},
			}
		},
		buildTextPart: func(text string) any {
			return map[string]any{
				"type": "text",
				"text": text,
			}
		},
		contentParts: func(body map[string]any) []any {
			return body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		},
		expectedTextType:    "text",
		expectedImageType:   "image_url",
		expectedFileIDError: "Only file.file_data is supported on this endpoint.",
		expectedFieldName:   "file.file_data",
	},
}

func newRC(visionCapable bool, process fileInputProcessor) *rewriteContext {
	return &rewriteContext{
		isMultimodal: func(string) bool { return visionCapable },
		model:        "test-model",
		process:      process,
	}
}

func nopProcessor(t *testing.T) fileInputProcessor {
	t.Helper()
	return func(context.Context, string, string, string, []byte, manager.FileConversionMode) (*manager.ConvertedFile, error) {
		t.Fatal("processor should not be called")
		return nil, nil
	}
}

func injectTinfoilMode(part map[string]any, value string) {
	if file, ok := part["file"].(map[string]any); ok {
		file[tinfoilModeFieldName] = value
		return
	}
	part[tinfoilModeFieldName] = value
}

func tinfoilModeFieldLeaked(part map[string]any) bool {
	if file, ok := part["file"].(map[string]any); ok {
		_, ok := file[tinfoilModeFieldName]
		return ok
	}
	_, ok := part[tinfoilModeFieldName]
	return ok
}

func TestRewriteBase64FilesWithProcessorText(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := tc.buildBody(
				tc.buildFileDataPart("notes.txt", "data:text/plain;base64,"+base64.StdEncoding.EncodeToString([]byte("hello"))),
				tc.buildTextPart("Summarize it."),
			)

			rewritten, err := tc.run(context.Background(), body, "Bearer test-key", newRC(false, nopProcessor(t)))
			if err != nil {
				t.Fatalf("rewrite failed: %v", err)
			}
			if !rewritten {
				t.Fatal("expected request body to be rewritten")
			}

			content := tc.contentParts(body)
			assertTextPart(t, content[0], tc.expectedTextType, "[Attached file: notes.txt]\n\nhello")
			assertPartType(t, content[1], tc.expectedTextType)
		})
	}
}

func TestRewriteBase64FilesWithProcessorRejectsFileID(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := tc.buildBody(tc.buildFileIDPart("file-123"))

			_, err := tc.run(context.Background(), body, "Bearer test-key", newRC(false, nopProcessor(t)))
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.expectedFileIDError {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDecodeFileInputRawBase64(t *testing.T) {
	decoded, err := decodeFileInput("report.md", base64.StdEncoding.EncodeToString([]byte("# hello")), "input_file.file_data")
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if decoded.filename != "report.md" {
		t.Fatalf("expected report.md, got %q", decoded.filename)
	}
	if decoded.contentType != "" {
		t.Fatalf("expected empty content type, got %q", decoded.contentType)
	}
	if string(decoded.data) != "# hello" {
		t.Fatalf("unexpected decoded payload: %q", string(decoded.data))
	}
}

func TestRewriteBase64FilesUsesContextSpecificDecodeErrors(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tests := []struct {
				name          string
				fileData      string
				expectedError string
			}{
				{
					name:          "invalid_data_url",
					fileData:      "data:text/plain;base64",
					expectedError: "Invalid data URL in " + tc.expectedFieldName + ".",
				},
				{
					name:          "missing_base64_marker",
					fileData:      "data:text/plain,hello",
					expectedError: tc.expectedFieldName + " must be base64-encoded.",
				},
				{
					name:          "invalid_base64_payload",
					fileData:      "data:text/plain;base64,%%%%",
					expectedError: "Invalid base64 payload in " + tc.expectedFieldName + ".",
				},
			}

			for _, test := range tests {
				test := test
				t.Run(test.name, func(t *testing.T) {
					body := tc.buildBody(tc.buildFileDataPart("notes.txt", test.fileData))

					_, err := tc.run(context.Background(), body, "Bearer test-key", newRC(false, nopProcessor(t)))
					if err == nil {
						t.Fatal("expected an error")
					}
					if err.Error() != test.expectedError {
						t.Fatalf("unexpected error: %v", err)
					}
				})
			}
		})
	}
}

func TestRewriteBase64FilesUsesProcessorForBinaryFiles(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := tc.buildBody(
				tc.buildFileDataPart("scan.pdf", "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString([]byte("%PDF"))),
			)

			called := false
			rc := newRC(false, func(_ context.Context, authHeader, filename, contentType string, data []byte, mode manager.FileConversionMode) (*manager.ConvertedFile, error) {
				called = true
				if authHeader != "Bearer test-key" {
					t.Fatalf("expected auth header to be forwarded, got %q", authHeader)
				}
				if filename != "scan.pdf" {
					t.Fatalf("expected scan.pdf, got %q", filename)
				}
				if contentType != "application/pdf" {
					t.Fatalf("expected application/pdf, got %q", contentType)
				}
				if string(data) != "%PDF" {
					t.Fatalf("unexpected payload: %q", string(data))
				}
				if mode != manager.FileConversionModeText {
					t.Fatalf("expected default text mode for non-multimodal model, got %q", mode)
				}
				return &manager.ConvertedFile{MDContent: "converted markdown"}, nil
			})

			rewritten, err := tc.run(context.Background(), body, "Bearer test-key", rc)
			if err != nil {
				t.Fatalf("rewrite failed: %v", err)
			}
			if !rewritten {
				t.Fatal("expected request body to be rewritten")
			}
			if !called {
				t.Fatal("expected processor to be called for binary input")
			}

			content := tc.contentParts(body)
			assertTextPart(t, content[0], tc.expectedTextType, "[Attached file: scan.pdf]\n\nconverted markdown")
		})
	}
}

func TestRewriteBase64FilesHonorsTinfoilModeOverride(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			part := tc.buildFileDataPart("scan.pdf", "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString([]byte("%PDF"))).(map[string]any)
			injectTinfoilMode(part, "vlm")
			body := tc.buildBody(part)

			seen := manager.FileConversionMode("")
			rc := newRC(false, func(_ context.Context, _, _, _ string, _ []byte, mode manager.FileConversionMode) (*manager.ConvertedFile, error) {
				seen = mode
				return &manager.ConvertedFile{MDContent: "ocr text"}, nil
			})

			if _, err := tc.run(context.Background(), body, "Bearer test-key", rc); err != nil {
				t.Fatalf("rewrite failed: %v", err)
			}
			if seen != manager.FileConversionModeVLM {
				t.Fatalf("expected vlm mode to reach processor, got %q", seen)
			}
			if tinfoilModeFieldLeaked(part) {
				t.Fatal("tinfoil_mode leaked into outgoing body")
			}
		})
	}
}

func TestRewriteBase64FilesRejectsImagesModeOnTextOnlyModel(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			part := tc.buildFileDataPart("scan.pdf", "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString([]byte("%PDF"))).(map[string]any)
			injectTinfoilMode(part, "images")
			body := tc.buildBody(part)

			_, err := tc.run(context.Background(), body, "Bearer test-key", newRC(false, nopProcessor(t)))
			if err == nil {
				t.Fatal("expected images-on-text-only to error")
			}
			var inputErr *fileInputError
			if !errors.As(err, &inputErr) {
				t.Fatalf("expected fileInputError, got %T", err)
			}
			if inputErr.StatusCode != 400 {
				t.Fatalf("expected status 400, got %d", inputErr.StatusCode)
			}
		})
	}
}

func TestRewriteBase64FilesRejectsUnknownTinfoilMode(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			part := tc.buildFileDataPart("scan.pdf", "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString([]byte("%PDF"))).(map[string]any)
			injectTinfoilMode(part, "supercharged")
			body := tc.buildBody(part)

			_, err := tc.run(context.Background(), body, "Bearer test-key", newRC(false, nopProcessor(t)))
			if err == nil {
				t.Fatal("expected unknown tinfoil_mode to error")
			}
			var inputErr *fileInputError
			if !errors.As(err, &inputErr) {
				t.Fatalf("expected fileInputError, got %T", err)
			}
			if inputErr.StatusCode != 400 {
				t.Fatalf("expected status 400, got %d", inputErr.StatusCode)
			}
		})
	}
}

func TestRewriteBase64FilesEmitsMultimodalContentForImagesMode(t *testing.T) {
	for _, tc := range rewriteTestCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			part := tc.buildFileDataPart("doc.pdf", "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString([]byte("%PDF"))).(map[string]any)
			injectTinfoilMode(part, "images")
			body := tc.buildBody(part)

			rc := newRC(true, func(_ context.Context, _, _, _ string, _ []byte, mode manager.FileConversionMode) (*manager.ConvertedFile, error) {
				if mode != manager.FileConversionModeImages {
					t.Fatalf("expected images mode, got %q", mode)
				}
				return &manager.ConvertedFile{
					Pages: []manager.ConvertedPage{
						{Page: 1, Text: "page one text", Image: "AAA="},
						{Page: 2, IsScanned: true, Image: "BBB="},
					},
				}, nil
			})

			if _, err := tc.run(context.Background(), body, "Bearer test-key", rc); err != nil {
				t.Fatalf("rewrite failed: %v", err)
			}

			parts := tc.contentParts(body)
			if len(parts) != 5 {
				t.Fatalf("expected file header + 2*(text+image) = 5 parts, got %d", len(parts))
			}
			assertTextPart(t, parts[0], tc.expectedTextType, "[Attached file: doc.pdf]")
			assertTextPart(t, parts[1], tc.expectedTextType, "Page 1:\npage one text")
			assertPartType(t, parts[2], tc.expectedImageType)
			assertTextPart(t, parts[3], tc.expectedTextType, "Page 2 (scanned):")
			assertPartType(t, parts[4], tc.expectedImageType)

			urlGetter := responsesImagePartURL
			if tc.name == "chat_completions" {
				urlGetter = chatImagePartURL
			}
			if got := urlGetter(parts[2]); got != "data:image/png;base64,AAA=" {
				t.Fatalf("page 1 image url = %q", got)
			}
			if got := urlGetter(parts[4]); got != "data:image/png;base64,BBB=" {
				t.Fatalf("page 2 image url = %q", got)
			}
		})
	}
}

func TestIsInlineTextFileHandlesContentTypeParameters(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		contentType string
		data        []byte
		expected    bool
	}{
		{
			name:        "text_with_charset",
			filename:    "notes.bin",
			contentType: "text/plain; charset=utf-8",
			data:        []byte("hello"),
			expected:    true,
		},
		{
			name:        "json_with_charset",
			filename:    "payload.bin",
			contentType: "application/json;charset=utf-8",
			data:        []byte(`{"ok":true}`),
			expected:    true,
		},
		{
			name:        "pdf_with_parameters",
			filename:    "scan.bin",
			contentType: "application/pdf; charset=utf-8",
			data:        []byte("%PDF"),
			expected:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isInlineTextFile(tt.filename, tt.contentType, tt.data); got != tt.expected {
				t.Fatalf("isInlineTextFile(%q, %q, %q) = %v, want %v", tt.filename, tt.contentType, string(tt.data), got, tt.expected)
			}
		})
	}
}

func assertTextPart(t *testing.T, rawPart any, expectedType string, expectedText string) {
	t.Helper()

	part := rawPart.(map[string]any)
	if got := part["type"]; got != expectedType {
		t.Fatalf("expected %s, got %v", expectedType, got)
	}
	if got := part["text"]; got != expectedText {
		t.Fatalf("unexpected rewritten text: %v", got)
	}
}

func assertPartType(t *testing.T, rawPart any, expectedType string) {
	t.Helper()

	part := rawPart.(map[string]any)
	if got := part["type"]; got != expectedType {
		t.Fatalf("expected %s, got %v", expectedType, got)
	}
}
