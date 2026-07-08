package cacheroute

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestMetricContract pins the metric surface Grafana panels are built on:
// every name and label set must appear in the exposition exactly as listed.
// A rename or label change here breaks dashboards and must be deliberate.
func TestMetricContract(t *testing.T) {
	s, clock, reg := newTestShadow(t)
	model := "contract-" + t.Name()
	cfg := defaultSettings()

	// Touch every metric: one ineligible request, several keyed ones
	// (repeat + hot enough to exist), one recovered panic, one capacity
	// path via the info setters.
	s.Observe(model, &Request{Outcome: OutcomeNoSalt}, pool2(), "enclave-a", cfg)
	req := keyedRequest(t, "contract", 1)
	for range 3 {
		clock.advance(time.Second)
		s.Observe(model, req, pool2(), "enclave-a", cfg)
	}
	s.Observe(model, nil, pool2(), "enclave-a", cfg) // recovered panic
	SetPoolInfo(model, []string{"enclave-a", "enclave-b"})
	SetMode(model, nil)
	t.Cleanup(func() { DropModel(model) })

	// Materialize zero-valued children for counters whose samples depend
	// on traffic specifics (an eviction happening, a pick agreeing with
	// random) — the contract under test is names and label sets, not
	// values.
	KeyEvictionsTotal.WithLabelValues(model, "ttl")
	RandomMatchTotal.WithLabelValues(model)

	want := map[string][]string{
		"router_cache_route_requests_total":           {"model", "outcome"},
		"router_cache_route_reuse_total":              {"model", "outcome"},
		"router_cache_route_reuse_prompt_bytes_total": {"model", "outcome"},
		"router_cache_route_picks_total":              {"model", "enclave", "r"},
		"router_cache_route_random_match_total":       {"model"},
		"router_cache_route_repeat_interval_seconds":  {"model"},
		"router_cache_route_key_rpm":                  {"model"},
		"router_cache_route_key_evictions_total":      {"model", "reason"},
		"router_cache_route_compute_seconds":          {},
		"router_cache_route_pool_info":                {"model", "pool_hash"},
		"router_cache_route_mode":                     {"model", "mode"},
		"router_cache_route_tracked_keys":             {"model", "enclave", "r"},
	}

	// The static metrics live on the default registerer (promauto); the
	// tracked-keys collector lives on the Shadow's registry.
	found := map[string][]string{}
	for _, g := range []prometheus.Gatherer{prometheus.DefaultGatherer, reg} {
		families, err := g.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range families {
			name := mf.GetName()
			if _, wanted := want[name]; !wanted {
				continue
			}
			var labels []string
			for _, lp := range mf.GetMetric()[0].GetLabel() {
				labels = append(labels, lp.GetName())
			}
			found[name] = labels
		}
	}

	for name, wantLabels := range want {
		gotLabels, ok := found[name]
		if !ok {
			t.Errorf("contract metric %s missing from exposition", name)
			continue
		}
		got := map[string]bool{}
		for _, l := range gotLabels {
			got[l] = true
		}
		if len(got) != len(wantLabels) {
			t.Errorf("%s labels = %v, want %v", name, gotLabels, wantLabels)
			continue
		}
		for _, l := range wantLabels {
			if !got[l] {
				t.Errorf("%s labels = %v, want %v", name, gotLabels, wantLabels)
			}
		}
	}
	// The eviction counter has no samples in this scenario (nothing
	// evicted), so it is exercised separately: it must at least be
	// registered under the contract name via its vec type.
	if _, err := KeyEvictionsTotal.GetMetricWithLabelValues(model, "capacity"); err != nil {
		t.Errorf("key_evictions_total unusable: %v", err)
	}
}
