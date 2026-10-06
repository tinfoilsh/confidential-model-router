package manager

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDocumentProcessingErrorSurvivesProxyNormalization(t *testing.T) {
	const message = "Document processing exceeded the time limit. Try splitting the document into smaller files."
	const code = "document_parser_timeout"
	// The document service retains its legacy error string alongside the flat
	// OpenAI fields so both direct and proxied uploads can consume the response.
	body := `{"error":"` + message + `","object":"error","message":"` + message + `","type":"server_error","code":"` + code + `","param":null}`
	response := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	normalizeUpstreamErrorResponse(response, "doc-upload", "document.example.test")
	defer response.Body.Close()
	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadGateway || envelope.Error.Message != message ||
		envelope.Error.Code == nil || *envelope.Error.Code != code || envelope.Error.Param != nil || envelope.Error.Type != ErrTypeServer {
		t.Fatalf("document classification was lost: %+v", envelope.Error)
	}
	converted := upstreamDocumentError(http.StatusBadGateway, nil, []byte(body))
	if converted.Code != code || converted.Message != errMsgDocumentFailed {
		t.Fatalf("internal file conversion lost its safe code or message policy: %+v", converted)
	}
}
