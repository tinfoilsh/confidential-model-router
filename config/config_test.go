package config

import (
	"fmt"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestReservationTierPolicyConsistency(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, third string
		wantErr                    bool
	}{
		{name: "same tier policy", first: "{soft_max_requests_running: 3}", second: "{soft_max_requests_running: 3}", third: "{soft_max_requests_running: 3}"},
		{name: "inherited tier policy", first: "null", second: "{soft_max_requests_running: 6, max_requests_waiting: 12}", third: "null"},
		{name: "equivalent defaults", first: "{max_requests_waiting: 4}", second: "{max_requests_waiting: 4, clear_requests_waiting: 2, retry_after_minutes: 1}", third: "{max_requests_waiting: 4, clear_requests_waiting: 4, retry_after_minutes: -1}"},
		{name: "disabled queue settings", first: "{soft_max_requests_running: 3}", second: "{soft_max_requests_running: 3, clear_requests_waiting: 2, retry_after_minutes: 2}", third: "{soft_max_requests_running: 3, max_requests_waiting: -1}"},
		{name: "disabled policies", first: "{}", second: "{soft_max_requests_running: -1, max_requests_waiting: -1}", third: "{clear_requests_waiting: 2, retry_after_minutes: 2}"},
		{name: "conflicting limits", first: "{soft_max_requests_running: 3}", second: "{soft_max_requests_running: 6}", third: "null", wantErr: true},
		{name: "conflicting clear marks", first: "{max_requests_waiting: 4}", second: "{max_requests_waiting: 4, clear_requests_waiting: 1}", third: "{max_requests_waiting: 4}", wantErr: true},
		{name: "conflicting retry delays", first: "{max_requests_waiting: 4}", second: "{max_requests_waiting: 4, retry_after_minutes: 2}", third: "{max_requests_waiting: 4}", wantErr: true},
		{name: "conflicting earlier queue policy", first: "{max_requests_waiting: 1}", second: "{}", third: "{soft_max_requests_running: 3}", wantErr: true},
		{name: "conflicting queue policies", wantErr: true, first: "{max_requests_waiting: 1}", second: "{max_requests_waiting: 12}", third: "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fmt.Sprintf(`models:
  gpt-oss-120b:
    overload: {soft_max_requests_running: 6, max_requests_waiting: 12}
    enclaves: [a.example, b.example, c.example]
    reservations:
      - {org_ids: [example-org], enclaves: [a.example], overload: %s}
      - {org_ids: [example-org], enclaves: [b.example], overload: %s}
      - {org_ids: [example-org], enclaves: [c.example], overload: %s}
`, tc.first, tc.second, tc.third)
			if _, err := FromBytes([]byte(data)); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

// A legacy rate_limit block must load like any other unknown field so a new
// router can serve from the same runtime YAML as the old fleet it replaces.
func TestLegacyRateLimitIgnored(t *testing.T) {
	for _, value := range []string{"null", "0", "{}", "{max_requests_per_minute: 0}", "{max_requests_per_minute: 10, hard_max_requests_per_minute: 20}"} {
		t.Run(value, func(t *testing.T) {
			cfg, err := FromBytes([]byte("models:\n  gpt-oss-120b:\n    repo: org/repo\n    enclaves: [a.example]\n    rate_limit: " + value + "\n"))
			if err != nil {
				t.Fatalf("legacy policy %q rejected: %v", value, err)
			}
			if m := cfg.Models["gpt-oss-120b"]; m.Repo != "org/repo" || len(m.Hostnames) != 1 {
				t.Fatalf("model fields lost alongside legacy policy: %+v", m)
			}
		})
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

func TestReservationOverloadParsing(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		want        *OverloadConfig
	}{
		{name: "omitted"},
		{name: "null", block: "        overload: null\n"},
		{name: "empty", block: "        overload: {}\n", want: &OverloadConfig{}},
		{name: "disabled", block: "        overload: {max_requests_waiting: 0}\n", want: &OverloadConfig{}},
		{name: "independent defaults", block: "        overload: {max_requests_waiting: 1}\n", want: &OverloadConfig{MaxRequestsWaiting: 1}},
		{name: "all fields", block: "        overload: {max_requests_waiting: 3, clear_requests_waiting: 1, retry_after_minutes: 2}\n", want: &OverloadConfig{MaxRequestsWaiting: 3, ClearRequestsWaiting: 1, RetryAfterMinutes: 2}},
		{name: "running only", block: "        overload: {soft_max_requests_running: 4}\n", want: &OverloadConfig{SoftMaxRequestsRunning: 4}},
		{name: "both limits", block: "        overload: {max_requests_waiting: 1, soft_max_requests_running: 4, retry_after_minutes: 2}\n", want: &OverloadConfig{MaxRequestsWaiting: 1, SoftMaxRequestsRunning: 4, RetryAfterMinutes: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("models:\n  gpt-oss-120b:\n    repo: org/repo\n    enclaves: [a.example]\n    overload: {max_requests_waiting: 14, clear_requests_waiting: 7, retry_after_minutes: 3}\n    reservations:\n      - org_ids: [org_test]\n        enclaves: [a.example]\n" + tc.block)
			for round := range 2 {
				cfg, err := FromBytes(data)
				if err != nil {
					t.Fatal(err)
				}
				got := cfg.Models["gpt-oss-120b"].Reservations[0].Overload
				if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
					t.Fatalf("round %d: reservation overload = %+v, want %+v", round, got, tc.want)
				}
				data, err = yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOverloadRunningConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     *OverloadConfig
		enabled bool
	}{
		{name: "omitted"},
		{name: "disabled", cfg: &OverloadConfig{}},
		{name: "negative disables", cfg: &OverloadConfig{SoftMaxRequestsRunning: -1}},
		{name: "waiting only", cfg: &OverloadConfig{MaxRequestsWaiting: 1}, enabled: true},
		{name: "running only", cfg: &OverloadConfig{SoftMaxRequestsRunning: 4}, enabled: true},
		{name: "both limits", cfg: &OverloadConfig{MaxRequestsWaiting: 1, SoftMaxRequestsRunning: 4}, enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Enabled(); got != tc.enabled {
				t.Fatalf("Enabled() = %v, want %v", got, tc.enabled)
			}
		})
	}
}
