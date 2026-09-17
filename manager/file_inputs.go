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
	errMsgDocumentFailedDetail    = "Document processing failed: %s"
)

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
		return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentUnavailable)
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fileConversionError(http.StatusBadGateway, errMsgDocumentReadResponse)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, upstreamDocumentError(resp.StatusCode, respBody)
	}

	return parseDocUploadResponse(respBody, mode)
}

// upstreamDocumentError surfaces a doc-upload enclave failure. The enclave's
// response text is included so clients see why their document was rejected,
// bounded so an unexpected body cannot balloon the error response. Non-2xx
// statuses other than client faults are reported as 502: the enclave's own
// 5xx codes describe its internals, not the router's.
func upstreamDocumentError(status int, respBody []byte) *APIError {
	detail := strings.TrimSpace(string(respBody))
	if len(detail) > maxDocumentErrorDetailBytes {
		detail = detail[:maxDocumentErrorDetailBytes] + "..."
	}
	if status < 400 || status >= 500 {
		status = http.StatusBadGateway
	}
	if detail == "" {
		return fileConversionError(status, errMsgDocumentFailed)
	}
	return fileConversionError(status, fmt.Sprintf(errMsgDocumentFailedDetail, detail))
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
