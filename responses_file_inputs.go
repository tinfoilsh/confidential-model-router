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

const tinfoilModeFieldName = "tinfoil_mode"

type fileInputProcessor func(
	ctx context.Context,
	authHeader string,
	filename string,
	contentType string,
	data []byte,
	mode manager.FileConversionMode,
) (*manager.ConvertedFile, error)

type fileInputError struct {
	StatusCode int
	Message    string
}

func (e *fileInputError) Error() string {
	return e.Message
}

type rewriteContext struct {
	isMultimodal func(modelName string) bool
	model        string
	process      fileInputProcessor
}

func newRewriteContext(em *manager.EnclaveManager, modelName string) *rewriteContext {
	return &rewriteContext{
		isMultimodal: em.IsMultimodal,
		model:        modelName,
		process: func(
			ctx context.Context,
			authHeader string,
			filename string,
			contentType string,
			data []byte,
			mode manager.FileConversionMode,
		) (*manager.ConvertedFile, error) {
			return em.ConvertFile(ctx, authHeader, filename, contentType, data, mode)
		},
	}
}

func rewriteResponsesBase64Files(
	ctx context.Context,
	body map[string]any,
	em *manager.EnclaveManager,
	authHeader string,
	modelName string,
) (bool, error) {
	return rewriteResponsesBase64FilesWithProcessor(ctx, body, authHeader, newRewriteContext(em, modelName))
}

func rewriteResponsesBase64FilesWithProcessor(
	ctx context.Context,
	body map[string]any,
	authHeader string,
	rc *rewriteContext,
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
			decoded, mode, err := decodeResponsesInputFilePart(part)
			if err != nil {
				return false, err
			}
			parts, err := renderDecodedFile(ctx, authHeader, decoded, mode, rc, responsesPartShape)
			if err != nil {
				return false, err
			}
			newContent = append(newContent, parts...)
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
	modelName string,
) (bool, error) {
	return rewriteChatCompletionsBase64FilesWithProcessor(ctx, body, authHeader, newRewriteContext(em, modelName))
}

func rewriteChatCompletionsBase64FilesWithProcessor(
	ctx context.Context,
	body map[string]any,
	authHeader string,
	rc *rewriteContext,
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

			decoded, mode, err := decodeChatCompletionsFilePart(part)
			if err != nil {
				return false, err
			}
			parts, err := renderDecodedFile(ctx, authHeader, decoded, mode, rc, chatPartShape)
			if err != nil {
				return false, err
			}
			newContent = append(newContent, parts...)
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

func decodeResponsesInputFilePart(part map[string]any) (*decodedFileInput, manager.FileConversionMode, error) {
	filename, _ := part["filename"].(string)
	if _, hasFileID := part["file_id"]; hasFileID {
		return nil, "", &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Only input_file.file_data is supported on this endpoint.",
		}
	}
	fileData, _ := part["file_data"].(string)
	if fileData == "" {
		return nil, "", &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Missing required parameter: 'file_data' on input_file.",
		}
	}
	mode, err := readTinfoilModeOverride(part)
	if err != nil {
		return nil, "", err
	}
	decoded, err := decodeFileInput(filename, fileData, "input_file.file_data")
	if err != nil {
		return nil, "", err
	}
	return decoded, mode, nil
}

func decodeChatCompletionsFilePart(part map[string]any) (*decodedFileInput, manager.FileConversionMode, error) {
	fileObj, _ := part["file"].(map[string]any)
	if fileObj == nil {
		return nil, "", &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Missing required parameter: 'file' on file content part.",
		}
	}
	filename, _ := fileObj["filename"].(string)
	if _, hasFileID := fileObj["file_id"]; hasFileID {
		return nil, "", &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Only file.file_data is supported on this endpoint.",
		}
	}
	fileData, _ := fileObj["file_data"].(string)
	if fileData == "" {
		return nil, "", &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    "Missing required parameter: 'file.file_data'.",
		}
	}
	mode, err := readTinfoilModeOverride(fileObj)
	if err != nil {
		return nil, "", err
	}
	decoded, err := decodeFileInput(filename, fileData, "file.file_data")
	if err != nil {
		return nil, "", err
	}
	return decoded, mode, nil
}

// readTinfoilModeOverride consumes the per-file tinfoil_mode field if
// present so it never leaks upstream to OpenAI-shaped backends.
func readTinfoilModeOverride(holder map[string]any) (manager.FileConversionMode, error) {
	raw, ok := holder[tinfoilModeFieldName]
	if !ok {
		return "", nil
	}
	delete(holder, tinfoilModeFieldName)

	switch v := raw.(type) {
	case nil:
		return "", nil
	case string:
		mode := manager.FileConversionMode(strings.TrimSpace(strings.ToLower(v)))
		if mode == "auto" {
			return "", nil
		}
		if !mode.IsValid() {
			return "", &fileInputError{
				StatusCode: http.StatusBadRequest,
				Message:    fmt.Sprintf("Invalid %s value: %q. Valid values are auto, text, vision, images, raw, vlm.", tinfoilModeFieldName, v),
			}
		}
		return mode, nil
	default:
		return "", &fileInputError{
			StatusCode: http.StatusBadRequest,
			Message:    fmt.Sprintf("%s must be a string.", tinfoilModeFieldName),
		}
	}
}

// resolveFileMode returns the doc-upload mode to use for a single attachment.
// The bool reports whether the file is inline text (in which case mode is
// irrelevant and the data is forwarded as-is).
func resolveFileMode(
	decoded *decodedFileInput,
	override manager.FileConversionMode,
	rc *rewriteContext,
) (manager.FileConversionMode, bool, error) {
	if isInlineTextFile(decoded.filename, decoded.contentType, decoded.data) {
		return "", true, nil
	}

	visionCapable := rc.isMultimodal != nil && rc.model != "" && rc.isMultimodal(rc.model)

	if override != "" {
		if override == manager.FileConversionModeImages && !visionCapable {
			return "", false, &fileInputError{
				StatusCode: http.StatusBadRequest,
				Message:    fmt.Sprintf("%s=images requires a vision-capable model; %q is text-only.", tinfoilModeFieldName, rc.model),
			}
		}
		return override, false, nil
	}

	if visionCapable {
		return manager.FileConversionModeImages, false, nil
	}
	return manager.FileConversionModeText, false, nil
}

// partShape captures the OpenAI-style content-part type names a given
// endpoint uses; the two endpoints differ in textType, imageType, and how
// the image URL itself is shaped.
type partShape struct {
	textType  string
	imagePart func(url string) map[string]any
}

var responsesPartShape = partShape{
	textType: "input_text",
	imagePart: func(url string) map[string]any {
		return map[string]any{
			"type":      "input_image",
			"image_url": url,
			"detail":    "auto",
		}
	},
}

var chatPartShape = partShape{
	textType: "text",
	imagePart: func(url string) map[string]any {
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url":    url,
				"detail": "auto",
			},
		}
	},
}

func renderDecodedFile(
	ctx context.Context,
	authHeader string,
	decoded *decodedFileInput,
	override manager.FileConversionMode,
	rc *rewriteContext,
	shape partShape,
) ([]any, error) {
	mode, inlineText, err := resolveFileMode(decoded, override, rc)
	if err != nil {
		return nil, err
	}

	textPart := func(text string) any {
		return map[string]any{"type": shape.textType, "text": text}
	}

	if inlineText {
		return []any{textPart(renderFileInputText(decoded.filename, string(decoded.data)))}, nil
	}

	result, err := rc.process(ctx, authHeader, decoded.filename, decoded.contentType, decoded.data, mode)
	if err != nil {
		return nil, err
	}

	if mode != manager.FileConversionModeImages {
		return []any{textPart(renderFileInputText(decoded.filename, result.MDContent))}, nil
	}

	parts := make([]any, 0, 1+2*len(result.Pages))
	parts = append(parts, textPart(fmt.Sprintf("[Attached file: %s]", decoded.filename)))
	for _, p := range result.Pages {
		parts = append(parts, textPart(renderPageHeader(p)))
		if p.Image != "" {
			parts = append(parts, shape.imagePart("data:image/png;base64,"+p.Image))
		}
	}
	return parts, nil
}

func renderPageHeader(p manager.ConvertedPage) string {
	label := fmt.Sprintf("Page %d", p.Page)
	if p.IsScanned {
		label += " (scanned)"
	}
	if strings.TrimSpace(p.Text) == "" {
		return label + ":"
	}
	return label + ":\n" + p.Text
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
