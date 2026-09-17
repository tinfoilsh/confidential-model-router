package manager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Error type strings returned in API error responses. These follow the
// OpenAI error taxonomy so client SDKs classify router errors the same way
// they classify upstream ones. See
// https://platform.openai.com/docs/guides/error-codes
const (
	ErrTypeInvalidRequest     = "invalid_request_error"
	ErrTypeRateLimit          = "rate_limit_error"
	ErrTypeServiceUnavailable = "service_unavailable_error"
	ErrTypeServer             = "server_error"
)

// Machine-readable error codes carried in the `code` field of API error
// responses. Codes shared with OpenAI keep OpenAI's spelling.
const (
	ErrCodeInvalidJSON        = "invalid_json"
	ErrCodeBodyReadFailed     = "body_read_failed"
	ErrCodeRequestTooLarge    = "request_too_large"
	ErrCodeMissingAPIKey      = "missing_api_key"
	ErrCodeMethodNotAllowed   = "method_not_allowed"
	ErrCodeModelNotFound      = "model_not_found"
	ErrCodeModelMismatch      = "model_mismatch"
	ErrCodeModelUnavailable   = "model_unavailable"
	ErrCodeRateLimitExceeded  = "rate_limit_exceeded"
	ErrCodeServerOverloaded   = "server_is_overloaded"
	ErrCodeUpstreamError      = "upstream_error"
	ErrCodeDocumentProcessing = "document_processing_failed"
)

// Client-facing error messages, aligned with OpenAI's standard error messages
// where applicable.
const (
	ErrMsgServerError      = "The server had an error while processing your request."
	ErrMsgModelNotFound    = "The model '%s' does not exist or you do not have access to it."
	ErrMsgModelUnavailable = "The model '%s' is temporarily unavailable. Please try again later."
	ErrMsgOverloaded       = "The model '%s' is currently overloaded. Retry after %d seconds."
	ErrMsgRateLimited      = "Rate limit reached for requests. Retry after %d seconds."
	ErrMsgBodyTooLarge     = "Request body is too large."
	ErrMsgBodyReadFailed   = "Could not read request body."
	ErrMsgInvalidJSON      = "Invalid request body: %v."
	ErrMsgInvalidBody      = "Invalid request body."
	ErrMsgMissingAPIKey    = "You didn't provide an API key."
	ErrMsgMethodNotAllowed = "Method not allowed."
	ErrMsgModelMismatch    = "The model in the request body does not match the " + ModelRequestHeader + " header."
	ErrMsgMissingParam     = "Missing required parameter: '%s'."
	ErrMsgInvalidParam     = "Invalid parameter: '%s' %s."
	ErrMsgAutoNoMultimodal = "Model 'auto' has no multimodal model available for image or file input."
	ErrMsgAutoNoScores     = "Model 'auto' is not available: no models publish intelligence scores."
	ErrMsgAutoUnavailable  = "Model 'auto' is not available for this request."
)

// APIError is an error response in OpenAI's format. Param and Code are
// emitted as JSON null when empty, matching OpenAI's envelope.
type APIError struct {
	Status  int
	Type    string
	Code    string
	Param   string
	Message string
}

func (e *APIError) Error() string {
	return e.Message
}

// WithMessage returns a copy with Message set to the formatted string.
func (e APIError) WithMessage(format string, args ...any) *APIError {
	e.Message = fmt.Sprintf(format, args...)
	return &e
}

// WithParam returns a copy with Param set.
func (e APIError) WithParam(param string) *APIError {
	e.Param = param
	return &e
}

// WithStatus returns a copy with Status set.
func (e APIError) WithStatus(status int) *APIError {
	e.Status = status
	return &e
}

// Predeclared errors. Parameterized messages are filled in with WithMessage
// at the call site.
var (
	ErrServer = APIError{
		Status:  http.StatusInternalServerError,
		Type:    ErrTypeServer,
		Message: ErrMsgServerError,
	}
	ErrUpstream = APIError{
		Status:  http.StatusBadGateway,
		Type:    ErrTypeServer,
		Code:    ErrCodeUpstreamError,
		Message: ErrMsgServerError,
	}
	ErrServerOverloaded = APIError{
		Status: http.StatusServiceUnavailable,
		Type:   ErrTypeServiceUnavailable,
		Code:   ErrCodeServerOverloaded,
	}
	ErrModelUnavailable = APIError{
		Status: http.StatusServiceUnavailable,
		Type:   ErrTypeServiceUnavailable,
		Code:   ErrCodeModelUnavailable,
	}
	ErrModelNotFound = APIError{
		Status: http.StatusNotFound,
		Type:   ErrTypeInvalidRequest,
		Code:   ErrCodeModelNotFound,
		Param:  "model",
	}
	ErrModelMismatch = APIError{
		Status:  http.StatusBadRequest,
		Type:    ErrTypeInvalidRequest,
		Code:    ErrCodeModelMismatch,
		Param:   "model",
		Message: ErrMsgModelMismatch,
	}
	ErrRateLimited = APIError{
		Status: http.StatusTooManyRequests,
		Type:   ErrTypeRateLimit,
		Code:   ErrCodeRateLimitExceeded,
	}
	ErrBodyTooLarge = APIError{
		Status:  http.StatusRequestEntityTooLarge,
		Type:    ErrTypeInvalidRequest,
		Code:    ErrCodeRequestTooLarge,
		Message: ErrMsgBodyTooLarge,
	}
	ErrBodyReadFailed = APIError{
		Status:  http.StatusBadRequest,
		Type:    ErrTypeInvalidRequest,
		Code:    ErrCodeBodyReadFailed,
		Message: ErrMsgBodyReadFailed,
	}
	ErrInvalidJSON = APIError{
		Status:  http.StatusBadRequest,
		Type:    ErrTypeInvalidRequest,
		Code:    ErrCodeInvalidJSON,
		Message: ErrMsgInvalidBody,
	}
	ErrMissingAPIKey = APIError{
		Status:  http.StatusUnauthorized,
		Type:    ErrTypeInvalidRequest,
		Code:    ErrCodeMissingAPIKey,
		Message: ErrMsgMissingAPIKey,
	}
	ErrMethodNotAllowed = APIError{
		Status:  http.StatusMethodNotAllowed,
		Type:    ErrTypeInvalidRequest,
		Code:    ErrCodeMethodNotAllowed,
		Message: ErrMsgMethodNotAllowed,
	}
	// ErrInvalidRequest is the base for parameter validation failures; call
	// sites set Message and Param.
	ErrInvalidRequest = APIError{
		Status: http.StatusBadRequest,
		Type:   ErrTypeInvalidRequest,
	}
)

// ErrorEnvelope is the JSON body of an API error response.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody mirrors OpenAI's error object. Param and Code serialize as null
// when unset.
type ErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Map returns the error object as a generic map, for embedding in SSE
// frames built from map[string]any.
func (b ErrorBody) Map() map[string]any {
	m := map[string]any{
		"message": b.Message,
		"type":    b.Type,
		"param":   nil,
		"code":    nil,
	}
	if b.Param != nil {
		m["param"] = *b.Param
	}
	if b.Code != nil {
		m["code"] = *b.Code
	}
	return m
}

// Envelope returns the JSON-serializable body for the error.
func (e *APIError) Envelope() ErrorEnvelope {
	return ErrorEnvelope{Error: ErrorBody{
		Message: e.Message,
		Type:    e.Type,
		Param:   nullableString(e.Param),
		Code:    nullableString(e.Code),
	}}
}

// WriteAPIError writes the error as a JSON response with its status code.
func WriteAPIError(w http.ResponseWriter, e *APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(e.Envelope())
}

// MaxUpstreamErrorBodyBytes bounds how much of a backend error response is
// buffered for normalization. Larger bodies are replaced by ErrUpstream.
const MaxUpstreamErrorBodyBytes = 64 << 10

// upstreamErrorTypes maps the error type names emitted by inference backends
// (vLLM uses Python exception class names) to OpenAI's error taxonomy.
// Types already in OpenAI's vocabulary pass through unchanged.
var upstreamErrorTypes = map[string]string{
	"BadRequestError":          ErrTypeInvalidRequest,
	"NotFoundError":            ErrTypeInvalidRequest,
	"ValidationError":          ErrTypeInvalidRequest,
	"UnprocessableEntityError": ErrTypeInvalidRequest,
	"InternalServerError":      ErrTypeServer,
	"ServiceUnavailableError":  ErrTypeServiceUnavailable,
	"RateLimitError":           ErrTypeRateLimit,
}

// NormalizeUpstreamError converts a backend's non-2xx response body into
// the OpenAI error envelope. It accepts both the nested
// {"error":{...}} shape and the flat {"object":"error",...} shape that
// older vLLM releases emit, keeps only the standard fields, maps backend
// type names onto OpenAI's, and drops numeric codes (the HTTP status
// already carries that). Bodies that are not recognizable JSON errors are
// logged by the caller and replaced with ErrUpstream at the given status,
// so backend internals never reach the client verbatim.
func NormalizeUpstreamError(status int, body []byte) (*APIError, bool) {
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return ErrUpstream.WithStatus(status), false
	}

	fields := parsed
	if inner, ok := parsed["error"].(map[string]any); ok {
		fields = inner
	} else if obj, _ := parsed["object"].(string); obj != "error" {
		return ErrUpstream.WithStatus(status), false
	}

	message, _ := fields["message"].(string)
	if message == "" {
		return ErrUpstream.WithStatus(status), false
	}

	errType, _ := fields["type"].(string)
	if mapped, ok := upstreamErrorTypes[errType]; ok {
		errType = mapped
	} else if !openAIErrorTypes[errType] {
		errType = errTypeForStatus(status)
	}

	code, _ := fields["code"].(string)
	param, _ := fields["param"].(string)

	return &APIError{
		Status:  status,
		Type:    errType,
		Code:    code,
		Param:   param,
		Message: message,
	}, true
}

// openAIErrorTypes is the set of error types OpenAI documents. Backend
// types outside this set and the mapping above are replaced by the type the
// HTTP status implies, so clients always see a classifiable value.
var openAIErrorTypes = map[string]bool{
	ErrTypeInvalidRequest:     true,
	ErrTypeRateLimit:          true,
	ErrTypeServiceUnavailable: true,
	ErrTypeServer:             true,
	"insufficient_quota":      true,
	"authentication_error":    true,
	"permission_error":        true,
	"not_found_error":         true,
}

// LogPreview returns a bounded, single-line rendering of a response body for
// diagnostic logs. Bodies are user-influenced, so the preview is capped and
// stripped of line breaks to keep log entries from being padded or forged.
func LogPreview(body []byte) string {
	const maxPreview = 256
	preview := body
	if len(preview) > maxPreview {
		preview = preview[:maxPreview]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, string(preview))
}

// errTypeForStatus picks the OpenAI error type implied by an HTTP status
// when the backend did not name one.
func errTypeForStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return ErrTypeRateLimit
	case status == http.StatusServiceUnavailable:
		return ErrTypeServiceUnavailable
	case status >= 500:
		return ErrTypeServer
	default:
		return ErrTypeInvalidRequest
	}
}
