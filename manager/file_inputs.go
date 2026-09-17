package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

// FileConversionMode is one of the modes accepted by /v1/convert/file. The
// empty string keeps the upstream default ("text").
type FileConversionMode string

const (
	FileConversionModeText   FileConversionMode = "text"
	FileConversionModeVision FileConversionMode = "vision"
	FileConversionModeImages FileConversionMode = "images"
	FileConversionModeRaw    FileConversionMode = "raw"
	FileConversionModeVLM    FileConversionMode = "vlm"
)

func (m FileConversionMode) IsValid() bool {
	switch m {
	case "", FileConversionModeText, FileConversionModeVision,
		FileConversionModeImages, FileConversionModeRaw, FileConversionModeVLM:
		return true
	}
	return false
}

// fileConversionError returns a document-processing APIError. 4xx statuses
// are the client's fault (invalid_request_error); everything else is a
// server_error.
func fileConversionError(status int, message string) *APIError {
	errType := ErrTypeServer
	if status >= 400 && status < 500 {
		errType = ErrTypeInvalidRequest
	}
	return &APIError{
		Status:  status,
		Type:    errType,
		Code:    ErrCodeDocumentProcessing,
		Message: message,
	}
}

type ConvertedPage struct {
	Page      int    `json:"page"`
	Text      string `json:"text"`
	Image     string `json:"image"`
	IsScanned bool   `json:"is_scanned"`
}

type ConvertedFile struct {
	MDContent string
	Pages     []ConvertedPage
}

type docUploadResponse struct {
	Document struct {
		MDContent string          `json:"md_content"`
		Pages     []ConvertedPage `json:"pages"`
	} `json:"document"`
}

func parseDocUploadResponse(respBody []byte, mode FileConversionMode) (*ConvertedFile, error) {
	var parsed docUploadResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fileConversionError(http.StatusBadGateway, "invalid document processing response")
	}

	out := &ConvertedFile{
		MDContent: parsed.Document.MDContent,
		Pages:     parsed.Document.Pages,
	}

	if mode == FileConversionModeImages {
		if len(out.Pages) == 0 {
			return nil, fileConversionError(http.StatusBadGateway, "document processing returned no pages")
		}
		return out, nil
	}

	if strings.TrimSpace(out.MDContent) == "" {
		return nil, fileConversionError(http.StatusBadGateway, "document processing returned empty content")
	}
	return out, nil
}

// ConvertFile sends an uploaded file to the private doc-upload enclave with
// the given mode and returns the parsed result.
func (em *EnclaveManager) ConvertFile(
	ctx context.Context,
	authHeader string,
	filename string,
	contentType string,
	data []byte,
	mode FileConversionMode,
) (*ConvertedFile, error) {
	model, found := em.GetModel("doc-upload")
	if !found {
		return nil, fileConversionError(http.StatusBadGateway, "document processing backend is not configured")
	}

	// This path uses its own client and records no breaker outcomes, so it
	// must not claim a recovery probe — a claimed probe with no recorded
	// outcome strands the breaker half-open until restart. Reservation
	// pools apply via the caller org in ctx.
	primary, spill := model.ReservationPools(CallerOrgFromContext(ctx))
	enclave, _ := model.selectForDispatchPools(nil, false, primary, spill)
	if enclave == nil {
		return nil, fileConversionError(http.StatusBadGateway, "document processing backend is unavailable")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, escapeMultipartFilename(filename)))
	if sanitizedContentType := sanitizeMultipartContentType(contentType); sanitizedContentType != "" {
		headers.Set("Content-Type", sanitizedContentType)
	}
	part, err := writer.CreatePart(headers)
	if err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, "failed to build document upload request")
	}
	if _, err := part.Write(data); err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, "failed to build document upload request")
	}
	if err := writer.WriteField("to_format", "md"); err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, "failed to build document upload request")
	}
	if err := writer.Close(); err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, "failed to finalize document upload request")
	}

	url := "https://" + enclave.host + "/v1/convert/file"
	if mode != "" {
		url += "?mode=" + string(mode)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, "failed to create document upload request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Content-Length", fmt.Sprintf("%d", body.Len()))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	client := &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &slowHeaderTripper{
			base: &tinfoilClient.TLSBoundRoundTripper{
				ExpectedPublicKey: enclave.tlsKeyFP,
			},
			timeout: responseHeaderTimeout,
			onSlow:  func() {},
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fileConversionError(http.StatusBadGateway, "document processing request failed")
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fileConversionError(http.StatusBadGateway, "failed to read document processing response")
	}
	if resp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = "document processing failed"
		}
		return nil, fileConversionError(resp.StatusCode, message)
	}

	return parseDocUploadResponse(respBody, mode)
}

func escapeMultipartFilename(filename string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		`"`, `\"`,
		"\r", "_",
		"\n", "_",
	)
	return replacer.Replace(filename)
}

func sanitizeMultipartContentType(contentType string) string {
	if contentType == "" || strings.ContainsAny(contentType, "\r\n") {
		return ""
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}

	return mime.FormatMediaType(mediaType, params)
}
