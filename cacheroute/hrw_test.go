package cacheroute

import (
	"fmt"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/cachesalt"
)

func testKey(i int) Key {
	return Key(cachesalt.Sum("tinfoil/test/v1", fmt.Sprintf("key-%d", i)))
}

func hosts(names ...string) []Candidate {
	c := make([]Candidate, len(names))
	for i, n := range names {
		c[i] = Candidate{Host: n}
	}
	return c
}

func rankedHosts(key Key, candidates []Candidate) []string {
	ranked := rank(key, candidates)
	out := make([]string, len(ranked))
	for i, c := range ranked {
		out[i] = c.Host
	}
	return out
}

func TestRankDeterministicAndNonMutating(t *testing.T) {
	candidates := hosts("c", "a", "d", "b")
	key := testKey(1)

	first := rankedHosts(key, candidates)
	for range 10 {
		if got := rankedHosts(key, candidates); fmt.Sprint(got) != fmt.Sprint(first) {
			t.Fatalf("ranking not deterministic: %v vs %v", got, first)
		}
	}
	// Input order must not matter (the score is order-free), and the input
	// slice must not be reordered.
	if got := rankedHosts(key, hosts("a", "b", "c", "d")); fmt.Sprint(got) != fmt.Sprint(first) {
		t.Fatalf("ranking depends on input order: %v vs %v", got, first)
	}
	if candidates[0].Host != "c" || candidates[3].Host != "b" {
		t.Fatal("rank mutated the caller's slice")
	}
}

// TestRankSubsetStability is HRW's defining property: removing a replica
// re-homes only the keys that ranked it first.
func TestRankSubsetStability(t *testing.T) {
	full := hosts("h1", "h2", "h3", "h4")
	for i := range 300 {
		key := testKey(i)
		ranked := rankedHosts(key, full)
		home, second := ranked[0], ranked[1]

		// Drop a non-home replica: home must not move.
		var withoutOther []Candidate
		for _, c := range full {
			if c.Host != ranked[3] {
				withoutOther = append(withoutOther, c)
			}
		}
		if got := rankedHosts(key, withoutOther)[0]; got != home {
			t.Fatalf("key %d re-homed from %s to %s when a non-home replica left", i, home, got)
		}

		// Drop the home: the new home must be the old second choice.
		var withoutHome []Candidate
		for _, c := range full {
			if c.Host != home {
				withoutHome = append(withoutHome, c)
			}
		}
		if got := rankedHosts(key, withoutHome)[0]; got != second {
			t.Fatalf("key %d: home fell to %s, want old rank-1 %s", i, got, second)
		}
	}
}

func TestRankDistribution(t *testing.T) {
	full := hosts("h1", "h2", "h3", "h4")
	counts := map[string]int{}
	const n = 4000
	for i := range n {
		counts[rankedHosts(testKey(i), full)[0]]++
	}
	for host, c := range counts {
		share := float64(c) / n
		if share < 0.18 || share > 0.32 {
			t.Errorf("host %s owns %.1f%% of homes, want ≈25%%", host, share*100)
		}
	}
}

func TestReplicationFactor(t *testing.T) {
	tests := []struct {
		rpm       float64
		threshold float64
		pool      int
		want      int
	}{
		{1, 15, 4, 1},
		{15, 15, 4, 1},
		{16, 15, 4, 2},
		{31, 15, 4, 3},
		{1000, 15, 4, 4}, // clamped to pool
		{1000, 15, 1, 1}, // single replica: nothing to split across
		{10, 0, 4, 1},    // degenerate threshold: never split
	}
	for _, tt := range tests {
		if got := replicationFactor(tt.rpm, tt.threshold, tt.pool); got != tt.want {
			t.Errorf("replicationFactor(%v, %v, %d) = %d, want %d", tt.rpm, tt.threshold, tt.pool, got, tt.want)
		}
	}
}

func TestLeastLoaded(t *testing.T) {
	ranked := []Candidate{{Host: "home", InFlight: 5}, {Host: "second", InFlight: 2}, {Host: "third", InFlight: 2}}
	if got := leastLoaded(ranked); got.Host != "second" {
		t.Errorf("leastLoaded = %s, want second (fewest in-flight, earliest rank on tie)", got.Host)
	}
	if got := leastLoaded(ranked[:1]); got.Host != "home" {
		t.Errorf("leastLoaded of one = %s, want home", got.Host)
	}
	even := []Candidate{{Host: "home", InFlight: 1}, {Host: "second", InFlight: 1}}
	if got := leastLoaded(even); got.Host != "home" {
		t.Errorf("tie must favor the higher-ranked candidate, got %s", got.Host)
	}
}
