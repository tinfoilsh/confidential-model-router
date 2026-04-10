package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

type fileInputProcessor func(ctx context.Context, authHeader string, filename string, contentType string, data []byte) (string, error)

type fileInputError struct {
	StatusCode int
	Message    string
}

func (e *fileInputError) Error() string {
	return e.Message
}

func rewriteResponsesBase64Files(
	ctx context.Context,
	body map[string]any,
	em *manager.EnclaveManager,
	authHeader string,
) (bool, error) {
	return rewriteResponsesBase64FilesWithProcessor(ctx, body, authHeader, em.ConvertFileToMarkdown)
}

func rewriteResponsesBase64FilesWithProcessor(
	ctx context.Context,
	body map[string]any,
	authHeader string,
	process fileInputProcessor,
) (bool, error) {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false, nil
	}

	rewritten := false
	for _, item := range inputItems {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}

		contentItems, ok := msg["content"].([]any)
		if !ok {
			continue
		}

		newContent := make([]any, 0, len(contentItems))
		for _, rawPart := range contentItems {
			part, ok := rawPart.(map[string]any)
			if !ok {
				newContent = append(newContent, rawPart)
				continue
			}

			partType, _ := part["type"].(string)
			if partType != "input_file" {
				newContent = append(newContent, rawPart)
				continue
			}
			decoded, err := decodeResponsesInputFilePart(part)
			if err != nil {
				return false, err
			}
			fileText, err := renderDecodedFile(ctx, authHeader, decoded, process)
			if err != nil {
				return false, err
			}

			newContent = append(newContent, map[string]any{
				"type": "input_text",
				"text": renderFileInputText(decoded.filename, fileText),
			})
			rewritten = true
		}

		msg["content"] = newContent
	}

	return rewritten, nil
}

func rewriteChatCompletionsBase64Files(
	ctx context.Context,
	body map[string]any,
	em *manager.EnclaveManager,
	authHeader string,
) (bool, error) {
	return rewriteChatCompletionsBase64FilesWithProcessor(ctx, body, authHeader, em.ConvertFileToMarkdown)
}

func rewriteChatCompletionsBase64FilesWithProcessor(
	ctx context.Context,
	body map[string]any,
	authHeader string,
	process fileInputProcessor,
) (bool, error) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return false, nil
	}

	rewritten := false
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}

		contentItems, ok := msg["content"].([]any)
		if !ok {
			continue
		}

		newContent := make([]any, 0, len(contentItems))
		for _, rawPart := range contentItems {
			part, ok := rawPart.(map[string]any)
			if !ok {
				newContent = append(newContent, rawPart)
				continue
			}

			partType, _ := part["type"].(string)
			if partType != "file" {
				newContent = append(newContent, rawPart)
				continue
			}

			decoded, err := decodeChatCompletionsFilePart(part)
			if err != nil {
				return false, err
			}
			fileText, err := renderDecodedFile(ctx, authHeader, decoded, process)
			if err != nil {
				return false, err
			}

			newContent = append(newContent, map[string]any{
				"type": "text",
				"text": renderFileInputText(decoded.filename, fileText),
			})
			rewritten = true
		}

		msg["content"] = newContent
	}

	return rewritten, nil
}

type decodedFileInput struct {
	filename    string
	contentType string
	data        []byte
}

func decodeResponsesInputFilePart(part map[string]any) (*decodedFileInput, error) {
	filename, _ := part["filename"].(string)
	if _, hasFileID := part["file_id"]; hasFileID {
		return nil, &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Only input_file.file_data is supported on this endpoint.",
		}
	}

	fileData, _ := part["file_data"].(string)
	if fileData == "" {
		return nil, &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Missing required parameter: 'file_data' on input_file.",
		}
	}

	return decodeFileInput(filename, fileData, "input_file.file_data")
}

func decodeChatCompletionsFilePart(part map[string]any) (*decodedFileInput, error) {
	fileObj, _ := part["file"].(map[string]any)
	if fileObj == nil {
		return nil, &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Missing required parameter: 'file' on file content part.",
		}
	}

	filename, _ := fileObj["filename"].(string)
	if _, hasFileID := fileObj["file_id"]; hasFileID {
		return nil, &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Only file.file_data is supported on this endpoint.",
		}
	}

	fileData, _ := fileObj["file_data"].(string)
	if fileData == "" {
		return nil, &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Missing required parameter: 'file.file_data'.",
		}
	}

	return decodeFileInput(filename, fileData, "file.file_data")
}

func renderDecodedFile(
	ctx context.Context,
	authHeader string,
	decoded *decodedFileInput,
	process fileInputProcessor,
) (string, error) {
	if isInlineTextFile(decoded.filename, decoded.contentType, decoded.data) {
		return string(decoded.data), nil
	}

	return process(ctx, authHeader, decoded.filename, decoded.contentType, decoded.data)
}

func decodeFileInput(filename string, fileData string, paramName string) (*decodedFileInput, error) {
	contentType := ""
	rawBase64 := fileData

	if strings.HasPrefix(fileData, "data:") {
		mediaType, payload, ok := strings.Cut(fileData, ",")
		if !ok {
			return nil, &fileInputError{
				StatusCode: http.StatusBadRequest,
				Message:    fmt.Sprintf("Invalid data URL in %s.", paramName),
			}
		}
		if !strings.HasSuffix(mediaType, ";base64") {
			return nil, &fileInputError{
				StatusCode: http.StatusBadRequest,
				Message:    fmt.Sprintf("%s must be base64-encoded.", paramName),
			}
		}
		contentType = strings.TrimPrefix(strings.TrimSuffix(mediaType, ";base64"), "data:")
		rawBase64 = payload
	}

	decoded, err := decodeBase64Payload(rawBase64)
	if err != nil {
		return nil, &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    fmt.Sprintf("Invalid base64 payload in %s.", paramName),
		}
	}
	if filename == "" {
		filename = "uploaded-file"
	}

	return &decodedFileInput{
		filename:    filename,
		contentType: contentType,
		data:        decoded,
	}, nil
}

func decodeBase64Payload(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("empty payload")
	}

	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err == nil {
		return decoded, nil
	}

	decoded, rawErr := base64.RawStdEncoding.DecodeString(trimmed)
	if rawErr == nil {
		return decoded, nil
	}

	return nil, err
}

func renderFileInputText(filename string, markdown string) string {
	if markdown == "" {
		return fmt.Sprintf("[Attached file: %s]", filename)
	}
	return fmt.Sprintf("[Attached file: %s]\n\n%s", filename, markdown)
}

func isInlineTextFile(filename string, contentType string, data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}

	if contentType != "" {
		mediaType := contentType
		if parsedMediaType, _, err := mime.ParseMediaType(contentType); err == nil {
			mediaType = parsedMediaType
		}

		if strings.HasPrefix(mediaType, "text/") {
			return true
		}
		switch mediaType {
		case "application/json", "application/xml", "application/yaml", "application/x-yaml":
			return true
		}
	}

	switch strings.ToLower(filepath.Ext(filename)) {
	case ".txt", ".md", ".json", ".xml", ".yaml", ".yml", ".csv", ".html", ".htm":
		return true
	}

	return false
}
