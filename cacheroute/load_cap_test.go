package cacheroute

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tinfoilsh/confidential-model-router/config"
)

func loaded(pairs ...any) []Candidate {
	c := make([]Candidate, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		c = append(c, Candidate{Host: pairs[i].(string), InFlight: pairs[i+1].(int)})
	}
	return c
}

func TestCappedPickDisabledMatchesLeastLoaded(t *testing.T) {
	ranked := loaded("warm", 9, "b", 3, "c", 0)
	host, demoted := cappedPick(ranked, 1, -1)
	if host != "warm" || demoted {
		t.Fatalf("disabled cap must keep the warm pick: got %s demoted=%v", host, demoted)
	}
}

func TestCappedPickUniformLoadKeepsWarm(t *testing.T) {
	// A uniformly busy pool must keep its warm picks: the cap is relative
	// to the pool minimum, not an absolute idleness requirement.
	ranked := loaded("warm", 12, "b", 12, "c", 12)
	host, demoted := cappedPick(ranked, 1, 0)
	if host != "warm" || demoted {
		t.Fatalf("uniform load must keep warm pick even at delta 0: got %s demoted=%v", host, demoted)
	}
}

func TestCappedPickDemotesOutlier(t *testing.T) {
	ranked := loaded("warm", 9, "b", 5, "c", 2)
	host, demoted := cappedPick(ranked, 1, 4)
	if host != "c" || !demoted {
		t.Fatalf("outlier warm home must demote to least-loaded: got %s demoted=%v", host, demoted)
	}
	// One request shallower and the warm pick is inside the cap again.
	ranked[0].InFlight = 6
	host, demoted = cappedPick(ranked, 1, 4)
	if host != "warm" || demoted {
		t.Fatalf("within-cap warm home must win: got %s demoted=%v", host, demoted)
	}
}

func TestCappedPickPrefersWithinCapWarmCopy(t *testing.T) {
	// A split key with one deep and one shallow warm copy serves at the
	// shallow copy — a warm hit, not a demotion.
	ranked := loaded("deep", 9, "shallow", 3, "cold", 1)
	host, demoted := cappedPick(ranked, 2, 4)
	if host != "shallow" || demoted {
		t.Fatalf("expected within-cap warm copy, got %s demoted=%v", host, demoted)
	}
}

func TestCappedPickTieFavorsWarmer(t *testing.T) {
	// Ties inside the warm set go to the higher-ranked (warmer) copy,
	// matching leastLoaded's behavior.
	ranked := loaded("warmest", 2, "second", 2, "cold", 0)
	host, demoted := cappedPick(ranked, 2, 4)
	if host != "warmest" || demoted {
		t.Fatalf("tie must favor the warmer copy: got %s demoted=%v", host, demoted)
	}
}

func TestDecideOrderSinksOverCapHosts(t *testing.T) {
	s := NewShadow(prometheus.NewRegistry())
	defer s.Close()

	pool := Pool{Size: 4, Candidates: loaded("a", 0, "b", 9, "c", 1, "d", 8)}
	cfg := Settings{Mode: ModeEnforced, Retention: DefaultRetention, SplitThresholdRPM: DefaultSplitThresholdRPM, MaxInflightDelta: 4}
	req := &Request{Outcome: OutcomeKeyed, Key: testKey(7), PromptBytes: 1024}

	d := s.Decide("m", req, pool, cfg)
	if d == nil {
		t.Fatal("expected a decision")
	}
	if len(d.Order) != 4 {
		t.Fatalf("order must contain every candidate once: %v", d.Order)
	}
	// b (9) and d (8) are over min(0)+4: they must trail a and c regardless
	// of their warmth rank.
	overCap := map[string]bool{"b": true, "d": true}
	if overCap[d.Order[0]] || overCap[d.Order[1]] {
		t.Fatalf("over-cap host ranked before within-cap hosts: %v", d.Order)
	}
	if !overCap[d.Order[2]] || !overCap[d.Order[3]] {
		t.Fatalf("over-cap hosts must sink to the tail: %v", d.Order)
	}
	if slices.Contains(d.Order[1:], d.Order[0]) {
		t.Fatalf("pick duplicated in order: %v", d.Order)
	}
}

func TestDecideDisabledCapKeepsFullRanking(t *testing.T) {
	s := NewShadow(prometheus.NewRegistry())
	defer s.Close()

	pool := Pool{Size: 3, Candidates: loaded("a", 0, "b", 9, "c", 1)}
	cfg := Settings{Mode: ModeEnforced, Retention: DefaultRetention, SplitThresholdRPM: DefaultSplitThresholdRPM, MaxInflightDelta: -1}
	req := &Request{Outcome: OutcomeKeyed, Key: testKey(7), PromptBytes: 1024}

	d := s.Decide("m", req, pool, cfg)
	if d == nil || len(d.Order) != 3 {
		t.Fatalf("expected full order, got %+v", d)
	}
}

func TestSettingsFromMaxInflightDelta(t *testing.T) {
	if got := SettingsFrom(nil).MaxInflightDelta; got != -1 {
		t.Fatalf("nil config must disable the cap, got %d", got)
	}
	if got := SettingsFrom(&config.CacheRouteConfig{Mode: "shadow"}).MaxInflightDelta; got != -1 {
		t.Fatalf("omitted field must disable the cap, got %d", got)
	}
	zero := 0
	if got := SettingsFrom(&config.CacheRouteConfig{Mode: "shadow", MaxInflightDelta: &zero}).MaxInflightDelta; got != 0 {
		t.Fatalf("explicit 0 must be the strictest cap, got %d", got)
	}
	four := 4
	if got := SettingsFrom(&config.CacheRouteConfig{Mode: "shadow", MaxInflightDelta: &four}).MaxInflightDelta; got != 4 {
		t.Fatalf("explicit 4 must carry through, got %d", got)
	}
	negative := -3
	if got := SettingsFrom(&config.CacheRouteConfig{Mode: "shadow", MaxInflightDelta: &negative}).MaxInflightDelta; got != -1 {
		t.Fatalf("negative must disable the cap, got %d", got)
	}
}
