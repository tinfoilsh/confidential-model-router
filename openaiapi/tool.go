package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Tool interface {
	ID() string
	GetParams(req *Request, endpoint Endpoint) (*ToolParams, ToolSession, error)
}

type ChatToolSpec = json.RawMessage
type ResponseToolSpec = json.RawMessage

type ToolParams struct {
	ChatTools              []ChatToolSpec
	ResponseTools          []ResponseToolSpec
	CallNames              []string
	StripFields            []string
	ResponseInterceptTypes []string
}

type ToolSession interface {
	Execute(ctx context.Context, call *ToolCall) (*ExecutionResult, error)
	Close(context.Context) error
}

type ToolCall struct {
	Endpoint Endpoint
	Name     string
	Value    map[string]any
	Raw      json.RawMessage
}

type ExecutionResult struct {
	ChatPatch           map[string]any
	ResponsesPublicItem map[string]any
	ResponsesReplayItem map[string]any
}

type activeTool struct {
	id      string
	params  *ToolParams
	session ToolSession
}

type ToolSet struct {
	ordered      []activeTool
	byCallName   map[string]ToolSession
	callNames    map[string]struct{}
	stripFields  map[string]struct{}
	responseSkip map[string]struct{}
}

func newToolSet() *ToolSet {
	return &ToolSet{
		byCallName:   map[string]ToolSession{},
		callNames:    map[string]struct{}{},
		stripFields:  map[string]struct{}{},
		responseSkip: map[string]struct{}{},
	}
}

func (s *ToolSet) Empty() bool {
	return s == nil || len(s.ordered) == 0
}

func (s *ToolSet) add(id string, params *ToolParams, session ToolSession) error {
	if s == nil {
		return fmt.Errorf("tool set is required")
	}
	if params == nil || session == nil {
		return fmt.Errorf("active tool %q must provide params and a session", id)
	}

	for _, name := range params.CallNames {
		name = stringsTrimSpace(name)
		if name == "" {
			return fmt.Errorf("active tool %q declared an empty call name", id)
		}
		if _, exists := s.callNames[name]; exists {
			return fmt.Errorf("built-in tool call name collision for %q", name)
		}
		s.callNames[name] = struct{}{}
		s.byCallName[name] = session
	}

	for _, field := range params.StripFields {
		field = stringsTrimSpace(field)
		if field != "" {
			s.stripFields[field] = struct{}{}
		}
	}
	for _, name := range params.ResponseInterceptTypes {
		name = stringsTrimSpace(name)
		if name != "" {
			s.responseSkip[name] = struct{}{}
		}
	}

	s.ordered = append(s.ordered, activeTool{
		id:      id,
		params:  params,
		session: session,
	})
	return nil
}

func (s *ToolSet) SessionForCall(name string) (ToolSession, bool) {
	if s == nil {
		return nil, false
	}
	session, ok := s.byCallName[stringsTrimSpace(name)]
	return session, ok
}

func (s *ToolSet) ChatTools() []json.RawMessage {
	if s == nil {
		return nil
	}
	var tools []json.RawMessage
	for _, active := range s.ordered {
		for _, spec := range active.params.ChatTools {
			tools = append(tools, cloneRawMessage(spec))
		}
	}
	return tools
}

func (s *ToolSet) ResponseTools() []json.RawMessage {
	if s == nil {
		return nil
	}
	var tools []json.RawMessage
	for _, active := range s.ordered {
		for _, spec := range active.params.ResponseTools {
			tools = append(tools, cloneRawMessage(spec))
		}
	}
	return tools
}

func (s *ToolSet) HasCallName(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.callNames[stringsTrimSpace(name)]
	return ok
}

func (s *ToolSet) ShouldStripField(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.stripFields[stringsTrimSpace(name)]
	return ok
}

func (s *ToolSet) InterceptsResponseToolType(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.responseSkip[stringsTrimSpace(name)]
	return ok
}

func (s *ToolSet) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var err error
	for _, active := range s.ordered {
		if active.session == nil {
			continue
		}
		err = errors.Join(err, active.session.Close(ctx))
	}
	return err
}

func stringsTrimSpace(value string) string {
	return strings.TrimSpace(value)
}
