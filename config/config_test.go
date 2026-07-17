package config

import "testing"

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
