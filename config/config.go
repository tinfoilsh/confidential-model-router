package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// RateLimitConfig describes optional per-API-key request rate limits for a model.
// When configured, requests from API keys that exceed the per-minute budget
// are sent to vLLM with a lower scheduling priority.
type RateLimitConfig struct {
	MaxRequestsPerMinute int64 `yaml:"max_requests_per_minute"`
}

// CacheRouteConfig is the per-model cache-aware routing knob. Mode is the
// rollout control: "off" (default), "shadow" (compute and meter the would-be
// routing decision without acting), or "enforced" (route keyed requests to
// their cache-aware pick). The remaining fields tune the measurement; zero
// means "use the cacheroute package default".
type CacheRouteConfig struct {
	Mode                   string  `yaml:"mode"`
	RetentionWindowMinutes int     `yaml:"retention_window_minutes,omitempty"`
	MinPromptBytes         int     `yaml:"min_prompt_bytes,omitempty"`
	SplitThresholdRPM      float64 `yaml:"split_threshold_rpm,omitempty"`
}

// Model represents the configuration for a single model
type Model struct {
	Repo       string            `yaml:"repo"`
	Hostnames  []string          `yaml:"enclaves"`
	Overload   *OverloadConfig   `yaml:"overload,omitempty"`
	RateLimit  *RateLimitConfig  `yaml:"rate_limit,omitempty"`
	CacheRoute *CacheRouteConfig `yaml:"cache_route,omitempty"`
}

// OverloadConfig describes optional overload thresholds for a model.
//
// MaxRequestsWaiting is the trip mark: a backend whose queue depth reaches it
// is flagged overloaded. ClearRequestsWaiting is the hysteresis clear mark:
// the flag clears only once the queue drains back down to it, so a backend
// hovering around the trip mark doesn't flap in and out of overload on every
// poll. Zero deliberately means omitted (the default of half the trip mark
// applies), so the smallest expressible clear mark is 1 — clear once at most
// one request is waiting. Queue depths are integral, so setting the clear
// mark to MaxRequestsWaiting-1 disables hysteresis.
type OverloadConfig struct {
	MaxRequestsWaiting   int `yaml:"max_requests_waiting"`
	ClearRequestsWaiting int `yaml:"clear_requests_waiting,omitempty"`
	RetryAfterMinutes    int `yaml:"retry_after_minutes"`
}

// Marks returns the overload trip and clear thresholds. When
// ClearRequestsWaiting is zero (omitted) or out of range (it must sit below
// the trip mark), the clear mark defaults to half the trip mark. Only
// meaningful when MaxRequestsWaiting is positive.
func (c *OverloadConfig) Marks() (trip, clear int) {
	trip = c.MaxRequestsWaiting
	clear = c.ClearRequestsWaiting
	if clear <= 0 || clear >= trip {
		clear = trip / 2
	}
	return trip, clear
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

// Loads configuration from a URL or file
func Load(url string, sha256_required bool) (*Config, error) {
	cleanURL, expectedSHA := parseURLWithSHA(url)
	log.Debugf("loading config from %s (sha256_required=%v)", cleanURL, sha256_required)
	if sha256_required {
		if expectedSHA == "" {
			return nil, fmt.Errorf("sha256 required for %s", url)
		}
	}
	var data []byte
	var err error
	if !strings.HasPrefix(cleanURL, "https") {
		if strings.HasPrefix(cleanURL, "http") {
			return nil, fmt.Errorf("http is not supported for config URLs. Use https instead.")
		}
		data, err = os.ReadFile(cleanURL)
		if err != nil {
			return nil, fmt.Errorf("failed to read runtime config: %w", err)
		}
	} else {
		resp, err := http.Get(cleanURL)
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

	// Optional integrity verification via URL fragment: #sha256=<hex>
	if expectedSHA != "" {
		sum := sha256.Sum256(data)
		actual := hex.EncodeToString(sum[:])
		if !strings.EqualFold(actual, expectedSHA) {
			return nil, fmt.Errorf("runtime config sha256 mismatch: expected %s, got %s", expectedSHA, actual)
		}
		log.Debug("config sha256 verification passed")
	}

	return FromBytes(data)
}

// parseURLWithSHA extracts an optional sha256 hash provided via fragment:
//
//	"<url-or-path>@sha256:<hex>"
//
// Returns the URL/path without the fragment and the expected hex string (or empty).
func parseURLWithSHA(input string) (string, string) {
	parts := strings.SplitN(input, "@", 2)
	if len(parts) == 1 {
		return input, ""
	}
	fragment := parts[1]
	const prefix = "sha256:"
	if strings.HasPrefix(strings.ToLower(fragment), prefix) {
		return parts[0], fragment[len(prefix):]
	}
	// Unknown fragment: ignore for loading purposes
	return parts[0], ""
}
