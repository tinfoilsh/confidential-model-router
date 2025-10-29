package config

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

// Model represents the configuration for a single model
type Model struct {
	Repo     string          `yaml:"repo"`
	Enclaves []string        `yaml:"enclaves"`
	Overload *OverloadConfig `yaml:"overload,omitempty"`
}

// OverloadConfig describes optional overload thresholds for a model.
type OverloadConfig struct {
	MaxRequestsWaiting int `yaml:"max_requests_waiting"`
	RetryAfterMinutes  int `yaml:"retry_after_minutes"`
}

// Config represents the configuration structure matching config.yml
type Config struct {
	Models map[string]Model `yaml:"models"`
}

// FromBytes parses a YAML config from bytes
func FromBytes(data []byte) (*Config, error) {
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse runtime config: %w", err)
	}

	return &config, nil
}

// Load loads configuration from a URL or file
func Load(url string) (*Config, error) {
	var data []byte
	var err error
	if !strings.HasPrefix(url, "http") {
		data, err = os.ReadFile(url)
		if err != nil {
			return nil, fmt.Errorf("failed to read runtime config: %w", err)
		}
	} else {
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch runtime config: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to fetch runtime config: HTTP %d", resp.StatusCode)
		}

		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read runtime config: %w", err)
		}
	}

	return FromBytes(data)
}
