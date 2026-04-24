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

type FileConversionError struct {
	StatusCode int
	Message    string
}

func (e *FileConversionError) Error() string {
	return e.Message
}

type docUploadResponse struct {
	Document struct {
		MDContent string `json:"md_content"`
	} `json:"document"`
}

func parseDocUploadMarkdown(respBody []byte) (string, error) {
	var parsed docUploadResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", &FileConversionError{
			StatusCode: http.StatusBadGateway,
			Message:    "invalid document processing response",
		}
	}

	if strings.TrimSpace(parsed.Document.MDContent) == "" {
		return "", &FileConversionError{
			StatusCode: http.StatusBadGateway,
			Message:    "document processing returned empty content",
		}
	}

	return parsed.Document.MDContent, nil
}

// ConvertFileToMarkdown sends an uploaded file to the private doc-upload enclave
// and returns the extracted markdown content.
func (em *EnclaveManager) ConvertFileToMarkdown(
	ctx context.Context,
	authHeader string,
	filename string,
	contentType string,
	data []byte,
) (string, error) {
	model, found := em.GetModel("doc-upload")
	if !found {
		return "", &FileConversionError{
			StatusCode: http.StatusBadGateway,
			Message:    "document processing backend is not configured",
		}
	}

	enclave := model.NextEnclave(nil)
	if enclave == nil {
		return "", &FileConversionError{
			StatusCode: http.StatusBadGateway,
			Message:    "document processing backend is unavailable",
		}
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
		return "", &FileConversionError{
			StatusCode: http.StatusInternalServerError,
			Message:    "failed to build document upload request",
		}
	}
	if _, err := part.Write(data); err != nil {
		return "", &FileConversionError{
			StatusCode: http.StatusInternalServerError,
			Message:    "failed to build document upload request",
		}
	}
	if err := writer.WriteField("to_format", "md"); err != nil {
		return "", &FileConversionError{
			StatusCode: http.StatusInternalServerError,
			Message:    "failed to build document upload request",
		}
	}
	if err := writer.Close(); err != nil {
		return "", &FileConversionError{
			StatusCode: http.StatusInternalServerError,
			Message:    "failed to finalize document upload request",
		}
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://"+enclave.host+"/v1/convert/file",
		bytes.NewReader(body.Bytes()),
	)
	if err != nil {
		return "", &FileConversionError{
			StatusCode: http.StatusInternalServerError,
			Message:    "failed to create document upload request",
		}
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
		return "", &FileConversionError{
			StatusCode: http.StatusBadGateway,
			Message:    "document processing request failed",
		}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", &FileConversionError{
			StatusCode: http.StatusBadGateway,
			Message:    "failed to read document processing response",
		}
	}
	if resp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = "document processing failed"
		}
		return "", &FileConversionError{
			StatusCode: resp.StatusCode,
			Message:    message,
		}
	}

	return parseDocUploadMarkdown(respBody)
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
