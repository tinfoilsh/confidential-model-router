package websearch

import (
	"encoding/json"
	"fmt"
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

func (t *Tool) Prepare(req *openaiapi.Request) (*openaiapi.PreparedRequest, error) {
	switch req.Kind {
	case openaiapi.EndpointChatCompletions:
		if !hasNonNullField(req.RawFields, "web_search_options") {
			return nil, nil
		}
		body, err := req.BodyBytes()
		if err != nil {
			return nil, err
		}
		return &openaiapi.PreparedRequest{
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

		fields := cloneRawFields(req.RawFields)
		toolsRaw, err := rawArray(fields["tools"])
		if err != nil {
			return nil, fmt.Errorf("invalid parameter: 'tools' must be an array")
		}

		filtered := make([]json.RawMessage, 0, len(toolsRaw))
		for idx, tool := range req.Responses.Params.Tools {
			if tool.OfWebSearch != nil || tool.OfWebSearchPreview != nil {
				continue
			}
			if idx < len(toolsRaw) {
				filtered = append(filtered, cloneRawMessage(toolsRaw[idx]))
			}
		}
		if len(filtered) == 0 {
			delete(fields, "tools")
		} else {
			encoded, err := json.Marshal(filtered)
			if err != nil {
				return nil, err
			}
			fields["tools"] = encoded
		}

		body, err := marshalRawFields(fields)
		if err != nil {
			return nil, err
		}
		return &openaiapi.PreparedRequest{
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
