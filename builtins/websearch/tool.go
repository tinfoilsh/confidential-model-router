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
		if err := normalizeResponsesToolChoice(fields, len(filtered)); err != nil {
			return nil, err
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

func normalizeResponsesToolChoice(fields map[string]json.RawMessage, remainingTools int) error {
	rawChoice, exists := fields["tool_choice"]
	if !exists {
		return nil
	}

	trimmed := strings.TrimSpace(string(rawChoice))
	if trimmed == "" || trimmed == "null" {
		delete(fields, "tool_choice")
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var mode string
		if err := json.Unmarshal(rawChoice, &mode); err != nil {
			return fmt.Errorf("invalid parameter: 'tool_choice' must be a string or object")
		}
		if strings.TrimSpace(mode) == "required" && remainingTools == 0 {
			fields["tool_choice"] = json.RawMessage(`"auto"`)
		}
		return nil
	}

	var choice map[string]any
	if err := json.Unmarshal(rawChoice, &choice); err != nil {
		return fmt.Errorf("invalid parameter: 'tool_choice' must be a string or object")
	}
	switch jsonString(choice["type"]) {
	case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
		fields["tool_choice"] = json.RawMessage(`"auto"`)
	}
	return nil
}

func jsonString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
