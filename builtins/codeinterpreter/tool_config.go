package codeinterpreter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ContainerConfig is the container configuration provided by the client request.
// Either ContainerID (pre-existing sandbox) or Auto (provision a new one) must be set.
type ContainerConfig struct {
	ContainerID string
	Auto        *AutoConfig
}

// AutoConfig describes how to auto-provision a sandbox.
type AutoConfig struct {
	TTLSeconds int32
}

// Private types for parsing container config from the API request.

type containerConfigParam struct {
	ContainerID string
	Auto        *autoContainerConfigParam
}

type autoContainerConfigParam struct {
	Type           string                       `json:"type,omitempty"`
	FileIDs        []string                     `json:"file_ids,omitempty"`
	NetworkPolicy  *containerNetworkPolicyParam `json:"network_policy,omitempty"`
	TTL            int32                        `json:"ttl,omitempty"`
	memoryLimitSet bool
	cpusSet        bool
}

type containerNetworkPolicyParam struct {
	Type string `json:"type,omitempty"`
}

type responsesToolTypeParam struct {
	Type string `json:"type"`
}

type responsesCodeInterpreterToolConfigParam struct {
	Type      string               `json:"type"`
	Container containerConfigParam `json:"container,omitempty"`
}

func parseChatContainerConfig(raw json.RawMessage) (ContainerConfig, error) {
	var params containerConfigParam
	if err := params.UnmarshalJSON(raw); err != nil {
		return ContainerConfig{}, err
	}
	return params.Validate(true)
}

func parseResponsesContainerConfig(rawTools []json.RawMessage) (ContainerConfig, bool, error) {
	var (
		config ContainerConfig
		found  bool
	)
	for _, toolRaw := range rawTools {
		toolType, err := parseResponsesToolType(toolRaw)
		if err != nil {
			return ContainerConfig{}, false, err
		}
		if toolType != ToolName {
			continue
		}
		if found {
			return ContainerConfig{}, false, fmt.Errorf("only one code_interpreter tool is supported in this increment")
		}
		var tool responsesCodeInterpreterToolConfigParam
		if err := json.Unmarshal(toolRaw, &tool); err != nil {
			return ContainerConfig{}, false, fmt.Errorf("invalid code interpreter tool")
		}
		config, err = tool.Validate()
		if err != nil {
			return ContainerConfig{}, false, err
		}
		found = true
	}
	return config, found, nil
}

func parseResponsesToolType(raw json.RawMessage) (string, error) {
	var tool responsesToolTypeParam
	if err := json.Unmarshal(raw, &tool); err != nil {
		return "", fmt.Errorf("invalid code interpreter tool")
	}
	return strings.TrimSpace(tool.Type), nil
}

func (p *containerConfigParam) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		*p = containerConfigParam{}
		return nil
	case trimmed[0] == '"':
		var containerID string
		if err := json.Unmarshal(trimmed, &containerID); err != nil {
			return fmt.Errorf("invalid code interpreter container config")
		}
		*p = containerConfigParam{ContainerID: strings.TrimSpace(containerID)}
		return nil
	case trimmed[0] == '{':
		var auto autoContainerConfigParam
		if err := json.Unmarshal(trimmed, &auto); err != nil {
			return fmt.Errorf("invalid code interpreter container config")
		}
		*p = containerConfigParam{Auto: &auto}
		return nil
	default:
		return fmt.Errorf("invalid code interpreter container config")
	}
}

func (p containerConfigParam) Validate(defaultAuto bool) (ContainerConfig, error) {
	containerID := strings.TrimSpace(p.ContainerID)
	if p.Auto == nil && containerID == "" {
		if defaultAuto {
			return ContainerConfig{Auto: &AutoConfig{}}, nil
		}
		return ContainerConfig{}, fmt.Errorf("code interpreter container config is required")
	}
	if p.Auto != nil {
		return p.Auto.Validate()
	}
	return ContainerConfig{ContainerID: containerID}, nil
}

func (p *autoContainerConfigParam) UnmarshalJSON(data []byte) error {
	type alias struct {
		Type          string                       `json:"type,omitempty"`
		FileIDs       []string                     `json:"file_ids,omitempty"`
		NetworkPolicy *containerNetworkPolicyParam `json:"network_policy,omitempty"`
		TTL           int32                        `json:"ttl,omitempty"`
	}
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*p = autoContainerConfigParam{
		Type:          decoded.Type,
		FileIDs:       decoded.FileIDs,
		NetworkPolicy: decoded.NetworkPolicy,
		TTL:           decoded.TTL,
	}
	_, p.memoryLimitSet = fields["memory_limit"]
	_, p.cpusSet = fields["cpus"]
	return nil
}

func (p autoContainerConfigParam) Validate() (ContainerConfig, error) {
	containerType := strings.TrimSpace(p.Type)
	if containerType == "" {
		containerType = "auto"
	}
	if containerType != "auto" {
		return ContainerConfig{}, fmt.Errorf("unsupported code interpreter container type %q", containerType)
	}
	if len(p.FileIDs) > 0 {
		return ContainerConfig{}, fmt.Errorf("code interpreter file_ids are not implemented in this increment")
	}
	if p.NetworkPolicy != nil {
		if err := p.NetworkPolicy.Validate(); err != nil {
			return ContainerConfig{}, err
		}
	}
	if p.memoryLimitSet {
		return ContainerConfig{}, fmt.Errorf("code interpreter memory_limit is not supported in this increment")
	}
	if p.cpusSet {
		return ContainerConfig{}, fmt.Errorf("code interpreter cpus is not supported in this increment")
	}
	return ContainerConfig{Auto: &AutoConfig{TTLSeconds: p.TTL}}, nil
}

func (p containerNetworkPolicyParam) Validate() error {
	if strings.TrimSpace(p.Type) != "disabled" {
		return fmt.Errorf("only code interpreter network_policy type \"disabled\" is supported in this increment")
	}
	return nil
}

func (p responsesCodeInterpreterToolConfigParam) Validate() (ContainerConfig, error) {
	return p.Container.Validate(true)
}
