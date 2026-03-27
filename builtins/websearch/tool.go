package websearch

import (
	"encoding/json"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/openaiapi"
)

const (
	ToolName       = "web_search"
	EffectiveModel = "websearch"
)

type Tool struct{}

func New() *Tool {
	return &Tool{}
}

func (t *Tool) ID() string {
	return ToolName
}

func (t *Tool) Prepare(req *openaiapi.Request) (*openaiapi.ProxyRequest, error) {
	switch req.Kind {
	case openaiapi.EndpointChatCompletions:
		if !hasNonNullField(req.RawFields, "web_search_options") {
			return nil, nil
		}
		body, err := req.BodyBytes()
		if err != nil {
			return nil, err
		}
		return &openaiapi.ProxyRequest{
			EffectiveModel: EffectiveModel,
			Body:           body,
		}, nil
	case openaiapi.EndpointResponses:
		if req == nil || req.Responses == nil {
			return nil, nil
		}

		hasBuiltin := false
		for _, tool := range req.Responses.Params.Tools {
			if tool.OfWebSearch != nil || tool.OfWebSearchPreview != nil {
				hasBuiltin = true
				break
			}
		}
		if !hasBuiltin {
			return nil, nil
		}
		body, err := req.BodyBytes()
		if err != nil {
			return nil, err
		}
		return &openaiapi.ProxyRequest{
			EffectiveModel: EffectiveModel,
			Body:           body,
		}, nil
	default:
		return nil, nil
	}
}

func hasNonNullField(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}
