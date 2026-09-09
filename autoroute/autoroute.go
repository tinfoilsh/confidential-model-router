// Package autoroute picks a concrete model and reasoning effort for requests
// that name the "auto" model. The caller states how capable a model it wants
// as an intelligence level; the router matches that against the per-effort
// intelligence scores the control plane publishes for each chat model and
// walks the closest matches until it finds one with a healthy backend.
package autoroute

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	// IntelligenceHeader carries the requested intelligence level when the
	// body does not.
	IntelligenceHeader = "X-Tinfoil-Intelligence"

	// OptionsField is the router-only body field that may carry the requested
	// intelligence level as {"intelligence": N}. It is always stripped from
	// the body before the request is forwarded.
	OptionsField = "auto_model_options"

	// IntelligenceKey is the key inside OptionsField that holds the level.
	IntelligenceKey = "intelligence"

	// MinIntelligence and MaxIntelligence bound the requested level, which is
	// normalized so that MaxIntelligence corresponds to the highest-scoring
	// model and effort currently in the catalog.
	MinIntelligence = 0
	MaxIntelligence = 100

	// DefaultIntelligence is used when the caller states no level.
	DefaultIntelligence = 50

	// effortPlaceholder is replaced inside a reasoning enable fragment with
	// the model's native effort value.
	effortPlaceholder = "$EFFORT"

	// effortOff selects a model with reasoning disabled; effortOn selects a
	// model whose reasoning is always enabled and has no effort knob.
	effortOff = "off"
	effortOn  = "on"

	// effortHigh is the strongest client-facing effort key.
	effortHigh = "high"

	// clientEffortField is the OpenAI chat completions effort parameter and
	// clientReasoningField/clientEffortKey its Responses API equivalent
	// (reasoning.effort). Both are stripped so the router's choice governs
	// every downstream reading of the request, not only the model call.
	clientEffortField    = "reasoning_effort"
	clientReasoningField = "reasoning"
	clientEffortKey      = "effort"
)

// visualPartTypes are the content part types that carry images or files a
// text-only model cannot read.
var visualPartTypes = map[string]bool{
	"image_url":   true,
	"input_image": true,
	"file":        true,
	"input_file":  true,
}

// EndpointParams holds the request fragments that switch a model's reasoning
// on or off for one API endpoint. Enable may contain the literal "$EFFORT"
// placeholder, which ApplyEffort replaces with the model's native effort.
type EndpointParams struct {
	Enable  map[string]any `json:"enable"`
	Disable map[string]any `json:"disable"`
}

// Reasoning describes how to apply a reasoning setting to a request for one
// model, as published by the control plane. Params is keyed by endpoint path;
// EffortMap translates the client-facing effort key (low, medium, high) to
// the model's native value.
type Reasoning struct {
	Params    map[string]EndpointParams `json:"params"`
	EffortMap map[string]string         `json:"effort_map"`
}

// Model is one entry in the routing catalog.
type Model struct {
	Name       string
	Multimodal bool
	Scores     map[string]int
	Reasoning  *Reasoning
}

// Candidate is one (model, effort) pair the router may select.
type Candidate struct {
	Model  Model
	Effort string
	// Score is the raw published intelligence score for this pair.
	Score int
	// Level is Score normalized to the MinIntelligence..MaxIntelligence range
	// against the highest score in the catalog.
	Level int
}

// ParseIntelligence extracts the requested intelligence level. The body's
// OptionsField object takes precedence over IntelligenceHeader, and an absent
// level falls back to DefaultIntelligence. The OptionsField is removed from
// the body regardless of shape so legacy payloads never reach the backend.
func ParseIntelligence(header http.Header, body map[string]any) (int, error) {
	raw, present := body[OptionsField]
	delete(body, OptionsField)
	if present {
		if options, ok := raw.(map[string]any); ok {
			if value, ok := options[IntelligenceKey]; ok {
				return intelligenceFromJSON(value)
			}
		}
	}
	if value := strings.TrimSpace(header.Get(IntelligenceHeader)); value != "" {
		level, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("Invalid %s header: must be an integer between %d and %d.", IntelligenceHeader, MinIntelligence, MaxIntelligence)
		}
		return validateIntelligence(level)
	}
	return DefaultIntelligence, nil
}

func intelligenceFromJSON(value any) (int, error) {
	number, ok := value.(float64)
	if !ok || number != math.Trunc(number) {
		return 0, fmt.Errorf("Invalid parameter: '%s.%s' must be an integer between %d and %d.", OptionsField, IntelligenceKey, MinIntelligence, MaxIntelligence)
	}
	return validateIntelligence(int(number))
}

func validateIntelligence(level int) (int, error) {
	if level < MinIntelligence || level > MaxIntelligence {
		return 0, fmt.Errorf("Invalid intelligence level %d: must be between %d and %d.", level, MinIntelligence, MaxIntelligence)
	}
	return level, nil
}

// HasVisualInput reports whether any message in the request carries an image
// or file content part, for both chat completions and Responses payloads.
func HasVisualInput(body map[string]any) bool {
	for _, field := range []string{"messages", "input"} {
		items, ok := body[field].([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			msg, ok := item.(map[string]any)
			if !ok {
				continue
			}
			parts, ok := msg["content"].([]any)
			if !ok {
				continue
			}
			for _, rawPart := range parts {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				if partType, _ := part["type"].(string); visualPartTypes[partType] {
					return true
				}
			}
		}
	}
	return false
}

// Rank expands the catalog into (model, effort) candidates and orders them by
// how closely their normalized level matches target. Ties prefer multimodal
// models, then the higher score, then the model name for determinism. When
// requireMultimodal is set, text-only models are excluded entirely.
func Rank(catalog []Model, target int, requireMultimodal bool) []Candidate {
	maxScore := 0
	for _, model := range catalog {
		for _, score := range model.Scores {
			if score > maxScore {
				maxScore = score
			}
		}
	}
	if maxScore == 0 {
		return nil
	}

	var candidates []Candidate
	for _, model := range catalog {
		if requireMultimodal && !model.Multimodal {
			continue
		}
		for effort, score := range model.Scores {
			candidates = append(candidates, Candidate{
				Model:  model,
				Effort: effort,
				Score:  score,
				Level:  normalize(score, maxScore),
			})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		da, db := abs(a.Level-target), abs(b.Level-target)
		if da != db {
			return da < db
		}
		if a.Model.Multimodal != b.Model.Multimodal {
			return a.Model.Multimodal
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Model.Name != b.Model.Name {
			return a.Model.Name < b.Model.Name
		}
		return a.Effort < b.Effort
	})
	return candidates
}

// ApplyEffort rewrites body so the selected candidate's reasoning setting is
// in effect for the given endpoint path. The router-chosen setting wins over
// any reasoning fields the client sent, so a caller asking for "auto" cannot
// accidentally pin an expensive effort: the client's generic effort fields
// are always removed, and the model's own fragment is merged in when it
// describes the endpoint.
func ApplyEffort(body map[string]any, path string, candidate Candidate) {
	delete(body, clientEffortField)
	if clientReasoning, ok := body[clientReasoningField].(map[string]any); ok {
		delete(clientReasoning, clientEffortKey)
		if len(clientReasoning) == 0 {
			delete(body, clientReasoningField)
		}
	}

	reasoning := candidate.Model.Reasoning
	if reasoning == nil {
		return
	}
	endpoint, ok := reasoning.Params[path]
	if !ok {
		return
	}

	var fragment map[string]any
	switch candidate.Effort {
	case effortOff:
		fragment = endpoint.Disable
	case effortOn:
		// Always-on models have no effort knob, but their enable fragment
		// may still carry a placeholder; fill it with the strongest effort.
		fragment = substituteEffort(endpoint.Enable, nativeEffort(reasoning, effortHigh)).(map[string]any)
	default:
		fragment = substituteEffort(endpoint.Enable, nativeEffort(reasoning, candidate.Effort)).(map[string]any)
	}
	if fragment == nil {
		return
	}
	mergeInto(body, fragment)
}

// nativeEffort translates a client-facing effort key to the model's native
// value, or returns the key unchanged when the model has no mapping for it.
func nativeEffort(reasoning *Reasoning, effort string) string {
	if mapped, ok := reasoning.EffortMap[effort]; ok {
		return mapped
	}
	return effort
}

// substituteEffort returns a deep copy of value with every effortPlaceholder
// string replaced by native.
func substituteEffort(value any, native string) any {
	switch v := value.(type) {
	case string:
		if v == effortPlaceholder {
			return native
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, inner := range v {
			out[key] = substituteEffort(inner, native)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = substituteEffort(inner, native)
		}
		return out
	default:
		return v
	}
}

// mergeInto deep-merges src into dst, recursing into nested objects present
// on both sides and overwriting everything else.
func mergeInto(dst, src map[string]any) {
	for key, value := range src {
		srcMap, srcIsMap := value.(map[string]any)
		dstMap, dstIsMap := dst[key].(map[string]any)
		if srcIsMap && dstIsMap {
			mergeInto(dstMap, srcMap)
			continue
		}
		if srcIsMap {
			copied := make(map[string]any, len(srcMap))
			mergeInto(copied, srcMap)
			dst[key] = copied
			continue
		}
		dst[key] = value
	}
}

func normalize(score, maxScore int) int {
	return int(math.Round(float64(score) * MaxIntelligence / float64(maxScore)))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
