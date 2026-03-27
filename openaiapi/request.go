package openaiapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

type Endpoint string

const (
	EndpointChatCompletions Endpoint = "chat_completions"
	EndpointResponses       Endpoint = "responses"
)

type ChatRequest struct {
	Params                 openai.ChatCompletionNewParams
	CodeInterpreterOptions json.RawMessage
}

type ResponsesRequest struct {
	Params responses.ResponseNewParams
}

type Request struct {
	Path                          string
	Kind                          Endpoint
	Model                         string
	Stream                        bool
	RawBody                       []byte
	RawFields                     map[string]json.RawMessage
	ClientRequestedStreamingUsage bool
	AuthToken                     string
	CallerAPIKeyID                string
	modified                      bool
	Chat                          *ChatRequest
	Responses                     *ResponsesRequest
}

func ParseRequest(path string, header http.Header, body []byte) (*Request, bool, error) {
	switch path {
	case "/v1/chat/completions":
		return parseChatCompletionsRequest(path, header, body)
	case "/v1/responses":
		return parseResponsesRequest(path, header, body)
	default:
		return nil, false, nil
	}
}

func parseChatCompletionsRequest(path string, header http.Header, body []byte) (*Request, bool, error) {
	fields, err := parseRawFields(body)
	if err != nil {
		return nil, true, err
	}

	var params openai.ChatCompletionNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		return nil, true, err
	}

	model := strings.TrimSpace(string(params.Model))
	if model == "" {
		return nil, true, fmt.Errorf("missing required parameter: 'model'")
	}

	stream, err := rawBool(fields["stream"])
	if err != nil {
		return nil, true, fmt.Errorf("invalid parameter: 'stream' must be a boolean")
	}

	streamOptions, err := rawObjectFields(fields["stream_options"])
	if err != nil {
		return nil, true, fmt.Errorf("invalid parameter: 'stream_options' must be an object")
	}

	authToken := bearerTokenFromHeader(header)
	return &Request{
		Path:                          path,
		Kind:                          EndpointChatCompletions,
		Model:                         model,
		Stream:                        stream,
		RawBody:                       append([]byte(nil), body...),
		RawFields:                     fields,
		ClientRequestedStreamingUsage: rawBoolValue(streamOptions["include_usage"]) || rawBoolValue(streamOptions["continuous_usage_stats"]),
		AuthToken:                     authToken,
		CallerAPIKeyID:                apiKeyID(authToken),
		Chat: &ChatRequest{
			Params:                 params,
			CodeInterpreterOptions: cloneRawMessage(fields["code_interpreter_options"]),
		},
	}, true, nil
}

func parseResponsesRequest(path string, header http.Header, body []byte) (*Request, bool, error) {
	fields, err := parseRawFields(body)
	if err != nil {
		return nil, true, err
	}

	var params responses.ResponseNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		return nil, true, err
	}

	model := strings.TrimSpace(string(params.Model))
	if model == "" {
		return nil, true, fmt.Errorf("missing required parameter: 'model'")
	}

	stream, err := rawBool(fields["stream"])
	if err != nil {
		return nil, true, fmt.Errorf("invalid parameter: 'stream' must be a boolean")
	}

	streamOptions, err := rawObjectFields(fields["stream_options"])
	if err != nil {
		return nil, true, fmt.Errorf("invalid parameter: 'stream_options' must be an object")
	}

	authToken := bearerTokenFromHeader(header)
	return &Request{
		Path:                          path,
		Kind:                          EndpointResponses,
		Model:                         model,
		Stream:                        stream,
		RawBody:                       append([]byte(nil), body...),
		RawFields:                     fields,
		ClientRequestedStreamingUsage: rawBoolValue(streamOptions["continuous_usage_stats"]),
		AuthToken:                     authToken,
		CallerAPIKeyID:                apiKeyID(authToken),
		Responses: &ResponsesRequest{
			Params: params,
		},
	}, true, nil
}

func (r *Request) StripPriority() bool {
	if r == nil {
		return false
	}
	if _, exists := r.RawFields["priority"]; !exists {
		return false
	}
	delete(r.RawFields, "priority")
	r.modified = true
	return true
}

func (r *Request) SetPriority(priority int) {
	if r == nil {
		return
	}
	r.RawFields["priority"] = json.RawMessage(strconv.Itoa(priority))
	r.modified = true
}

func (r *Request) EnableContinuousUsageStats() error {
	if r == nil || !r.Stream {
		return nil
	}

	streamOptions, err := rawObjectFields(r.RawFields["stream_options"])
	if err != nil {
		streamOptions = nil
	}
	if streamOptions == nil {
		streamOptions = map[string]json.RawMessage{}
	}

	if rawBoolValue(streamOptions["include_usage"]) || rawBoolValue(streamOptions["continuous_usage_stats"]) {
		r.ClientRequestedStreamingUsage = true
	}

	streamOptions["continuous_usage_stats"] = json.RawMessage("true")
	encoded, err := json.Marshal(streamOptions)
	if err != nil {
		return err
	}
	r.RawFields["stream_options"] = encoded
	r.modified = true
	return nil
}

func (r *Request) BodyBytes() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("request is required")
	}
	if !r.modified {
		return append([]byte(nil), r.RawBody...), nil
	}
	return marshalRawFields(r.RawFields)
}

func bearerTokenFromHeader(header http.Header) string {
	if header == nil {
		return ""
	}
	auth := strings.TrimSpace(header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

func apiKeyID(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

func parseRawFields(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	return fields, nil
}

func cloneRawFields(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, value := range src {
		dst[key] = cloneRawMessage(value)
	}
	return dst
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func marshalRawFields(fields map[string]json.RawMessage) ([]byte, error) {
	if fields == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(fields)
}

func rawBool(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, err
	}
	return value, nil
}

func rawBoolValue(raw json.RawMessage) bool {
	value, err := rawBool(raw)
	return err == nil && value
}

func rawObjectFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func rawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func hasNonNullField(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	return strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "null"
}
