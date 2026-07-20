package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// RateLimitConfig describes optional per-API-key rate limits for a model.
// Enforcement types (hard should be calculated first)
// soft max: requests over this are given a lower scheduling priority.
// hard max: requests over this are rejected completely.
// Also defines a configurable window per each ratelimit type
//
// Un-cached is handled unintuitively: token budgets are debited pesimistaclly on recieve of the requests.
// After a request finishes, the cache % is credited back.
type RateLimitConfig struct {
	MaxRequestsPerMinute     int64 `yaml:"max_requests_per_minute"`
	HardMaxRequestsPerMinute int64 `yaml:"hard_max_requests_per_minute,omitempty"`
	RequestsWindowMinutes    int64 `yaml:"requests_window_minutes,omitempty"`

	MaxUncachedPromptTokensPerMinute     int64 `yaml:"max_uncached_prompt_tokens_per_minute,omitempty"`
	HardMaxUncachedPromptTokensPerMinute int64 `yaml:"hard_max_uncached_prompt_tokens_per_minute,omitempty"`
	TokensWindowMinutes                  int64 `yaml:"tokens_window_minutes,omitempty"`
}

// TracksTokens reports whether any uncached-prompt-token budget is set.
func (c *RateLimitConfig) TracksTokens() bool {
	return c.MaxUncachedPromptTokensPerMinute > 0 || c.HardMaxUncachedPromptTokensPerMinute > 0
}

func windowDuration(minutes int64) time.Duration {
	if minutes < 1 {
		minutes = 1
	}
	return time.Duration(minutes) * time.Minute
}

// RequestsWindow is the enforcement window for the request-count axis.
func (c *RateLimitConfig) RequestsWindow() time.Duration {
	return windowDuration(c.RequestsWindowMinutes)
}

// TokensWindow is the enforcement window for the uncached-token axis.
func (c *RateLimitConfig) TokensWindow() time.Duration {
	return windowDuration(c.TokensWindowMinutes)
}

// Per-window budgets: the configured per-minute rate scaled by the window.

func (c *RateLimitConfig) SoftRequestsBudget() int64 {
	return c.MaxRequestsPerMinute * int64(c.RequestsWindow()/time.Minute)
}

func (c *RateLimitConfig) HardRequestsBudget() int64 {
	return c.HardMaxRequestsPerMinute * int64(c.RequestsWindow()/time.Minute)
}

func (c *RateLimitConfig) SoftTokensBudget() int64 {
	return c.MaxUncachedPromptTokensPerMinute * int64(c.TokensWindow()/time.Minute)
}

func (c *RateLimitConfig) HardTokensBudget() int64 {
	return c.HardMaxUncachedPromptTokensPerMinute * int64(c.TokensWindow()/time.Minute)
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

// ReservationConfig dedicates a subset of a model's enclaves to the listed
// orgs: only their traffic may use those enclaves, spilling to the shared
// pool when they are overloaded or unhealthy.
type ReservationConfig struct {
	OrgIDs   []string `yaml:"org_ids" json:"org_ids"`
	Enclaves []string `yaml:"enclaves" json:"enclaves"`
}

// Model represents the configuration for a single model
type Model struct {
	Repo         string              `yaml:"repo"`
	Hostnames    []string            `yaml:"enclaves"`
	Overload     *OverloadConfig     `yaml:"overload,omitempty"`
	RateLimit    *RateLimitConfig    `yaml:"rate_limit,omitempty"`
	CacheRoute   *CacheRouteConfig   `yaml:"cache_route,omitempty"`
	Reservations []ReservationConfig `yaml:"reservations,omitempty"`
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
