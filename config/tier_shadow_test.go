package config

import "testing"

func TestTierShadowThresholdsAreSeparateFromOverload(t *testing.T) {
	cfg, err := FromBytes([]byte(`models:
  gpt-oss-120b:
    overload: {max_requests_waiting: 14}
    tier_shadow: {max_requests_running: 8, max_requests_waiting: 14}
    reservations:
      - org_ids: [example-org]
        enclaves: [reserved.example]
        tier_shadow: {max_requests_running: 4, max_requests_waiting: 1}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models["gpt-oss-120b"]
	if m.Overload.MaxRequestsWaiting != 14 || m.Reservations[0].Overload != nil {
		t.Fatal("shadow settings changed live overload settings")
	}
	if m.TierShadow.MaxRequestsRunning != 8 || m.Reservations[0].TierShadow.MaxRequestsRunning != 4 {
		t.Fatal("shadow settings were not loaded")
	}
}
