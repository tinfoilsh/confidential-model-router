package manager

import (
	"encoding/json"
	"fmt"
	"net/http"
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
