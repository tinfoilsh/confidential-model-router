package codeinterpreter

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Args are the typed arguments the LLM provides for a code_interpreter tool call.
type Args struct {
	Code      string `json:"code"`
	ContextID string `json:"context_id,omitempty"`
}

func (a Args) Validate() error {
	if strings.TrimSpace(a.Code) == "" {
		return fmt.Errorf("parse code_interpreter arguments: code is required")
	}
	return nil
}

// ParseArgs parses raw JSON arguments from the LLM into typed Args.
func ParseArgs(raw string) (Args, error) {
	type rawPayload struct {
		Code      string `json:"code"`
		ContextID string `json:"context_id,omitempty"`
	}
	var payload rawPayload
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return Args{}, fmt.Errorf("parse code_interpreter arguments: %w", err)
	}
	args := Args{
		Code:      strings.TrimSpace(payload.Code),
		ContextID: strings.TrimSpace(payload.ContextID),
	}

	if err := args.Validate(); err != nil {
		return Args{}, err
	}
	return args, nil
}

// codeInterpreterParameters returns the JSON schema for code_interpreter tool calls.
// This is the contract exposed to the LLM.
func codeInterpreterParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "Python code to execute inside the reusable sandbox container.",
			},
			"context_id": map[string]any{
				"type":        "string",
				"description": "Optional Jupyter context ID to continue or branch interpreter state within the same sandbox. This refers to a notebook/kernel context, not a new sandbox.",
			},
		},
		"required":             []string{"code"},
		"additionalProperties": false,
	}
}
