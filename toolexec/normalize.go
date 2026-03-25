package toolexec

import (
	"fmt"

	"github.com/tinfoilsh/confidential-model-router/toolexec/codeinterpreter"
)

func parseContainerConfig(value any, defaultAuto bool) (codeinterpreter.ContainerConfig, error) {
	switch typed := value.(type) {
	case nil:
		if defaultAuto {
			return codeinterpreter.ContainerConfig{
				Auto: &codeinterpreter.AutoConfig{},
			}, nil
		}
		return codeinterpreter.ContainerConfig{}, fmt.Errorf("code interpreter container config is required")
	case string:
		if typed == "" {
			if defaultAuto {
				return codeinterpreter.ContainerConfig{
					Auto: &codeinterpreter.AutoConfig{},
				}, nil
			}
			return codeinterpreter.ContainerConfig{}, fmt.Errorf("code interpreter container config is required")
		}
		return codeinterpreter.ContainerConfig{ContainerID: typed}, nil
	case map[string]any:
		if len(typed) == 0 {
			return codeinterpreter.ContainerConfig{
				Auto: &codeinterpreter.AutoConfig{},
			}, nil
		}

		autoType := jsonString(typed["type"])
		if autoType == "" {
			autoType = "auto"
		}
		if autoType != "auto" {
			return codeinterpreter.ContainerConfig{}, fmt.Errorf("unsupported code interpreter container type %q", autoType)
		}

		fileIDs := rawJSONArray(typed["file_ids"])
		if len(fileIDs) > 0 {
			return codeinterpreter.ContainerConfig{}, fmt.Errorf("code interpreter file_ids are not implemented in this increment")
		}

		if networkPolicy := rawJSONMap(typed["network_policy"]); networkPolicy != nil {
			if jsonString(networkPolicy["type"]) != "disabled" {
				return codeinterpreter.ContainerConfig{}, fmt.Errorf("only code interpreter network_policy type \"disabled\" is supported in this increment")
			}
		}

		return codeinterpreter.ContainerConfig{
			Auto: &codeinterpreter.AutoConfig{
				MemoryLimit: jsonString(typed["memory_limit"]),
			},
		}, nil
	default:
		return codeinterpreter.ContainerConfig{}, fmt.Errorf("invalid code interpreter container config")
	}
}

func toolHasName(tool map[string]any, name string) bool {
	if tool == nil {
		return false
	}
	if jsonString(tool["type"]) == codeInterpreterToolName {
		return name == codeInterpreterToolName
	}
	if jsonString(tool["type"]) != "function" {
		return false
	}
	// Chat Completions API: name is nested inside a "function" object.
	if function := rawJSONMap(tool["function"]); function != nil {
		return jsonString(function["name"]) == name
	}
	// Responses API: name is at the top level of the tool object.
	return jsonString(tool["name"]) == name
}
