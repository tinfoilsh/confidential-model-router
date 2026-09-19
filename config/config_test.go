package config

import (
	"strings"
	"testing"
)

func TestRejectLegacyRateLimit(t *testing.T) {
	for _, value := range []string{"", "null", "~", "0", "false", "{}", "[]", "malformed", "{max_requests_per_minute: 0}", "{max_requests_per_minute: 10, hard_max_requests_per_minute: 20}"} {
		t.Run(value, func(t *testing.T) {
			_, err := FromBytes([]byte("models:\n  gpt-oss-120b:\n    repo: org/repo\n    rate_limit: " + value + "\n"))
			if err == nil || !strings.Contains(err.Error(), "rate_limit") {
				t.Fatalf("legacy policy %q: expected explicit rate_limit error, got %v", value, err)
			}
		})
	}
}

func TestRejectMergedLegacyRateLimit(t *testing.T) {
	_, err := FromBytes([]byte("defaults: &defaults {rate_limit: null}\nmodels:\n  gpt-oss-120b:\n    <<: *defaults\n    repo: org/repo\n"))
	if err == nil || !strings.Contains(err.Error(), "rate_limit") {
		t.Fatalf("merged legacy policy accepted: %v", err)
	}
}

func TestRuntimeConfigFeaturesRemainCompatible(t *testing.T) {
	cfg, err := FromBytes([]byte(`
future_runtime_setting: true
models:
  gpt-oss-120b:
    repo: org/repo
    enclaves: [a.example, b.example]
    future_model_setting: true
    overload: {max_requests_waiting: 10, clear_requests_waiting: 3, retry_after_minutes: 2}
    cache_route: {mode: enforced, max_inflight_delta: 0}
    reservations: [{org_ids: [org_test], enclaves: [a.example]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models["gpt-oss-120b"]
	if m.Repo != "org/repo" || len(m.Hostnames) != 2 || m.Overload == nil || m.Overload.ClearRequestsWaiting != 3 || m.CacheRoute == nil || m.CacheRoute.Mode != "enforced" || m.CacheRoute.MaxInflightDelta == nil || *m.CacheRoute.MaxInflightDelta != 0 || len(m.Reservations) != 1 || m.Reservations[0].OrgIDs[0] != "org_test" {
		t.Fatalf("runtime features lost: %+v", m)
	}
}

func TestOverloadConfigMarks(t *testing.T) {
	tests := []struct {
		name      string
		cfg       OverloadConfig
		wantTrip  int
		wantClear int
	}{
		{"unset clear defaults to half", OverloadConfig{MaxRequestsWaiting: 16}, 16, 8},
		{"odd trip rounds down", OverloadConfig{MaxRequestsWaiting: 9}, 9, 4},
		{"explicit clear honored", OverloadConfig{MaxRequestsWaiting: 16, ClearRequestsWaiting: 12}, 16, 12},
		{"clear of trip-1 disables hysteresis", OverloadConfig{MaxRequestsWaiting: 16, ClearRequestsWaiting: 15}, 16, 15},
		{"clear at trip falls back to default", OverloadConfig{MaxRequestsWaiting: 16, ClearRequestsWaiting: 16}, 16, 8},
		{"clear above trip falls back to default", OverloadConfig{MaxRequestsWaiting: 16, ClearRequestsWaiting: 20}, 16, 8},
		{"negative clear falls back to default", OverloadConfig{MaxRequestsWaiting: 16, ClearRequestsWaiting: -1}, 16, 8},
		{"trip of one clears only when empty", OverloadConfig{MaxRequestsWaiting: 1}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trip, clear := tt.cfg.Marks()
			if trip != tt.wantTrip || clear != tt.wantClear {
				t.Fatalf("Marks() = (%d, %d), want (%d, %d)", trip, clear, tt.wantTrip, tt.wantClear)
			}
		})
	}
}

func TestOverloadConfigParsesClearMark(t *testing.T) {
	cfg, err := FromBytes([]byte(`
models:
  test-model:
    repo: tinfoilsh/test
    enclaves:
      - test.example.com
    overload:
      max_requests_waiting: 16
      clear_requests_waiting: 12
      retry_after_minutes: 1
`))
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	overload := cfg.Models["test-model"].Overload
	if overload == nil {
		t.Fatal("overload config not parsed")
	}
	if overload.MaxRequestsWaiting != 16 || overload.ClearRequestsWaiting != 12 || overload.RetryAfterMinutes != 1 {
		t.Fatalf("parsed overload = %+v", overload)
	}
}

func TestCacheRouteConfigParsing(t *testing.T) {
	cfg, err := FromBytes([]byte(`
models:
  shadowed:
    repo: org/repo
    enclaves: [a.example, b.example]
    cache_route:
      mode: shadow
      retention_window_minutes: 5
      min_prompt_bytes: 2048
      split_threshold_rpm: 30
  plain:
    repo: org/repo
    enclaves: [c.example]
`))
	if err != nil {
		t.Fatal(err)
	}

	cr := cfg.Models["shadowed"].CacheRoute
	if cr == nil {
		t.Fatal("cache_route block not parsed")
	}
	if cr.Mode != "shadow" || cr.RetentionWindowMinutes != 5 || cr.MinPromptBytes != 2048 || cr.SplitThresholdRPM != 30 {
		t.Fatalf("cache_route = %+v", cr)
	}
	if cfg.Models["plain"].CacheRoute != nil {
		t.Fatal("absent cache_route must stay nil")
	}
}

func TestReservationConfigParsing(t *testing.T) {
	cfg, err := FromBytes([]byte(`
models:
  reserved:
    repo: org/repo
    enclaves: [a.example, b.example, c.example]
    reservations:
      - org_ids: [org_abc123, org_def456]
        enclaves: [c.example]
  plain:
    repo: org/repo
    enclaves: [d.example]
`))
	if err != nil {
		t.Fatal(err)
	}

	reservations := cfg.Models["reserved"].Reservations
	if len(reservations) != 1 {
		t.Fatalf("reservations = %+v, want one entry", reservations)
	}
	if len(reservations[0].OrgIDs) != 2 || reservations[0].OrgIDs[0] != "org_abc123" || reservations[0].OrgIDs[1] != "org_def456" {
		t.Fatalf("reservation orgs = %+v", reservations[0])
	}
	if len(reservations[0].Enclaves) != 1 || reservations[0].Enclaves[0] != "c.example" {
		t.Fatalf("reservation = %+v", reservations[0])
	}
	if cfg.Models["plain"].Reservations != nil {
		t.Fatal("absent reservations must stay nil")
	}
}
