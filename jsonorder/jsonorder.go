// Package jsonorder keeps the client's encoding of the request fields whose
// key order steers generation.
//
// The router decodes request bodies into map[string]any so it can rewrite
// them, and encoding/json writes maps with their keys sorted. vLLM's grammar
// backends generate JSON Schema properties in the order the schema lists
// them, so re-encoding a response_format or tool schema alphabetically
// changes the order in which the model writes its fields: a schema that asks
// for "reasoning" before "answer" would have the model commit to the answer
// first.
//
// Capture indexes the client's bytes for those fields by the canonical
// encoding encoding/json produces for the same value, and Marshal writes the
// client's bytes back wherever the router left a value unchanged. Matching on
// content rather than position survives the tool loop, which clones request
// bodies and appends its own tools.
package jsonorder

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"slices"
)

// fields are the top-level request fields that carry JSON Schemas:
// response_format on chat completions, text on responses, tool parameters on
// both, and vLLM's structured_outputs and guided_json extensions.
var fields = []string{"response_format", "text", "tools", "structured_outputs", "guided_json"}

// Original is the client's encoding of a request's schema-bearing fields,
// and of each element of those fields that are arrays, keyed by canonical
// encoding. A nil Original is valid and marshals exactly like encoding/json.
type Original struct {
	raw map[string]json.RawMessage
}

type contextKey struct{}

// Capture records the client's encoding of the schema-bearing fields in a
// request body, given the body and its decoded top-level object. It returns
// nil when the body carries none of them, or when the body is not strict JSON
// (duplicate names, invalid UTF-8), so that only bytes that decode to exactly
// the router's value are ever reused.
func Capture(body []byte, decoded map[string]any) *Original {
	if !slices.ContainsFunc(fields, func(name string) bool { _, ok := decoded[name]; return ok }) {
		return nil
	}
	dec := jsontext.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != jsontext.KindBeginObject {
		return nil
	}
	o := &Original{raw: make(map[string]json.RawMessage)}
	for dec.PeekKind() != jsontext.KindEndObject {
		name, err := dec.ReadToken()
		if err != nil {
			return nil
		}
		if !slices.Contains(fields, name.String()) {
			if err := dec.SkipValue(); err != nil {
				return nil
			}
			continue
		}
		value, err := dec.ReadValue()
		if err != nil {
			return nil
		}
		if err := o.add(value); err != nil {
			return nil
		}
	}
	if len(o.raw) == 0 {
		return nil
	}
	return o
}

// NewContext returns a copy of ctx carrying o, for dispatches that encode a
// request body far from where it was decoded, such as the tool loop.
func NewContext(ctx context.Context, o *Original) context.Context {
	return context.WithValue(ctx, contextKey{}, o)
}

// FromContext returns the Original carried by ctx, or nil.
func FromContext(ctx context.Context) *Original {
	o, _ := ctx.Value(contextKey{}).(*Original)
	return o
}

// lookup returns the client's compact encoding of v when v is canonically
// equal to a captured object or array.
func (o *Original) lookup(v any) (json.RawMessage, bool) {
	canonical, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	raw, ok := o.raw[string(canonical)]
	return raw, ok
}

// Marshal encodes body like json.Marshal, except that a schema-bearing field,
// or an element of one, that still equals what the client sent is written in
// the client's encoding. Values the router changed are encoded as they are.
func (o *Original) Marshal(body map[string]any) ([]byte, error) {
	if o == nil {
		return json.Marshal(body)
	}
	out := make(map[string]any, len(body))
	for name, value := range body {
		if slices.Contains(fields, name) {
			value = o.restore(value)
		}
		out[name] = value
	}
	return json.Marshal(out)
}

func (o *Original) restore(value any) any {
	if raw, ok := o.lookup(value); ok {
		return raw
	}
	items, ok := value.([]any)
	if !ok {
		return value
	}
	restored := make([]any, len(items))
	for i, item := range items {
		restored[i] = item
		if raw, ok := o.lookup(item); ok {
			restored[i] = raw
		}
	}
	return restored
}

// add indexes a captured field and, when it is an array, each element.
func (o *Original) add(value jsontext.Value) error {
	if err := o.index(value); err != nil {
		return err
	}
	if value.Kind() != jsontext.KindBeginArray {
		return nil
	}
	dec := jsontext.NewDecoder(bytes.NewReader(value))
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	for dec.PeekKind() != jsontext.KindEndArray {
		item, err := dec.ReadValue()
		if err != nil {
			return err
		}
		if err := o.index(item); err != nil {
			return err
		}
	}
	return nil
}

// index stores the compact client encoding of an object or array under its
// canonical encoding. Scalars have no key order to keep.
func (o *Original) index(value jsontext.Value) error {
	if kind := value.Kind(); kind != jsontext.KindBeginObject && kind != jsontext.KindBeginArray {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	raw := jsontext.Value(bytes.Clone(value))
	if err := raw.Compact(); err != nil {
		return err
	}
	o.raw[string(canonical)] = json.RawMessage(raw)
	return nil
}
