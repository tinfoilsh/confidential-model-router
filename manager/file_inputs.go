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

// maxDocumentErrorDetailBytes bounds how much of the doc-upload enclave's
// error body is echoed to the client.
const maxDocumentErrorDetailBytes = 512

// Client-facing messages for document processing failures.
const (
	errMsgDocumentInvalidResponse = "Document processing returned an invalid response."
	errMsgDocumentNoPages         = "Document processing returned no pages."
	errMsgDocumentEmpty           = "Document processing returned no text content."
	errMsgDocumentNotConfigured   = "Document processing is not available on this deployment."
	errMsgDocumentUnavailable     = "Document processing is temporarily unavailable. Please try again later."
	errMsgDocumentBuildRequest    = "Could not prepare the document for processing."
	errMsgDocumentRequestFailed   = "Document processing request failed."
	errMsgDocumentReadResponse    = "Could not read the document processing response."
	errMsgDocumentFailed          = "Document processing failed."
	errMsgDocumentRateLimited     = "Rate limit reached for document processing. Please retry later."
	errMsgDocumentFailedDetail    = "Document processing failed: %s"
)

// fileConversionError returns a document-processing APIError classified by status.
func fileConversionError(status int, message string) *APIError {
	return &APIError{
		Status:  status,
		Type:    errTypeForStatus(status),
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
		return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentInvalidResponse)
	}

	out := &ConvertedFile{
		MDContent: parsed.Document.MDContent,
		Pages:     parsed.Document.Pages,
	}

	if mode == FileConversionModeImages {
		if len(out.Pages) == 0 {
			return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentNoPages)
		}
		return out, nil
	}

	if strings.TrimSpace(out.MDContent) == "" {
		return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentEmpty)
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
		return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentNotConfigured)
	}

	// This path uses its own client and records no breaker outcomes, so it
	// must not claim a recovery probe — a claimed probe with no recorded
	// outcome strands the breaker half-open until restart. Reservation
	// pools apply via the caller org in ctx.
	primary, spill := model.ReservationPools(CallerOrgFromContext(ctx))
	enclave, _ := model.selectForDispatchPools(nil, false, primary, spill)
	if enclave == nil {
		return nil, ErrModelUnavailable.WithMessage(errMsgDocumentUnavailable)
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
		return nil, fileConversionError(http.StatusInternalServerError, errMsgDocumentBuildRequest)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, errMsgDocumentBuildRequest)
	}
	if err := writer.WriteField("to_format", "md"); err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, errMsgDocumentBuildRequest)
	}
	if err := writer.Close(); err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, errMsgDocumentBuildRequest)
	}

	url := "https://" + enclave.host + "/v1/convert/file"
	if mode != "" {
		url += "?mode=" + string(mode)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, fileConversionError(http.StatusInternalServerError, errMsgDocumentBuildRequest)
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
		return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentRequestFailed)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxUpstreamErrorBodyBytes+1))
		if err != nil || len(respBody) > MaxUpstreamErrorBodyBytes {
			respBody = nil
		}
		return nil, upstreamDocumentError(resp.StatusCode, resp.Header, respBody)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentReadResponse)
	}

	return parseDocUploadResponse(respBody, mode)
}

// upstreamDocumentError surfaces a doc-upload enclave failure. Client rejection
// details are included so clients see why their document was rejected,
// bounded so an unexpected body cannot balloon the error response. HTTP errors
// retain their status, structured classification, and valid retry hint.
func upstreamDocumentError(status int, header http.Header, respBody []byte) *APIError {
	if status < http.StatusBadRequest || status > maxHTTPErrorStatus {
		return fileConversionError(http.StatusBadGateway, errMsgDocumentFailed)
	}
	if normalized, ok := NormalizeUpstreamError(status, respBody); ok {
		if status >= http.StatusInternalServerError {
			normalized.Message = errMsgDocumentFailed
		}
		return normalized.WithRetryAfter(header)
	}
	if status >= http.StatusInternalServerError {
		return fileConversionError(status, errMsgDocumentFailed).WithRetryAfter(header)
	}
	if status == http.StatusTooManyRequests {
		return ErrRateLimited.WithMessage(errMsgDocumentRateLimited).WithRetryAfter(header)
	}
	detail := strings.TrimSpace(string(respBody))
	if len(detail) > maxDocumentErrorDetailBytes {
		detail = strings.ToValidUTF8(detail[:maxDocumentErrorDetailBytes], "") + "..."
	}
	if detail == "" {
		return fileConversionError(status, errMsgDocumentFailed).WithRetryAfter(header)
	}
	return fileConversionError(status, fmt.Sprintf(errMsgDocumentFailedDetail, detail)).WithRetryAfter(header)
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
