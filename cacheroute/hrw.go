package cacheroute

import (
	"math"
	"slices"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// Candidate is a replica the selector could pick: its stable enclave
// hostname (the HRW identity, which must survive redeploys or keys re-home
// on every release) and its live in-flight request count.
type Candidate struct {
	Host     string
	InFlight int
}

// Pool is a model's replica set at selection time. Size is the configured
// replica count. Candidates are the breaker-closed replicas, falling back
// to all replicas when every breaker is open.
type Pool struct {
	Size       int
	Candidates []Candidate
}

// rank orders candidates by rendezvous (HRW) score for key, highest first:
// score = xxhash64(key ‖ host). The key is fixed-width and already
// uniformly distributed, so no framing or cryptographic hash is needed.
// Ties break to the lexicographically smaller host so every router agrees.
// The caller's slice is not modified.
func rank(key Key, candidates []Candidate) []Candidate {
	ranked := slices.Clone(candidates)
	scores := make(map[string]uint64, len(ranked))
	var d xxhash.Digest
	for _, c := range ranked {
		d.Reset()
		d.Write(key[:])
		d.WriteString(c.Host)
		scores[c.Host] = d.Sum64()
	}
	slices.SortFunc(ranked, func(a, b Candidate) int {
		switch sa, sb := scores[a.Host], scores[b.Host]; {
		case sa > sb:
			return -1
		case sa < sb:
			return 1
		default:
			return strings.Compare(a.Host, b.Host)
		}
	})
	return ranked
}

// replicationFactor is the size of a key's warm set: roughly one replica
// per replica's-worth of traffic, at least 1, at most the pool.
func replicationFactor(rpm, thresholdRPM float64, poolSize int) int {
	if thresholdRPM <= 0 || poolSize <= 1 {
		return 1
	}
	r := int(math.Ceil(rpm / thresholdRPM))
	if r < 1 {
		r = 1
	}
	if r > poolSize {
		r = poolSize
	}
	return r
}

// leastLoaded picks the candidate with the fewest in-flight requests; ties
// go to the earlier (higher-ranked) entry, favoring the key's home.
func leastLoaded(ranked []Candidate) Candidate {
	best := ranked[0]
	for _, c := range ranked[1:] {
		if c.InFlight < best.InFlight {
			best = c
		}
	}
	return best
}

// minInFlight returns the smallest in-flight count among candidates.
func minInFlight(candidates []Candidate) int {
	m := candidates[0].InFlight
	for _, c := range candidates[1:] {
		if c.InFlight < m {
			m = c.InFlight
		}
	}
	return m
}

// cappedPick is leastLoaded over the key's warm set (ranked[:r]) subject to
// the load cap: a warm candidate is eligible only while it is at most delta
// requests deeper than the least-loaded candidate in the whole ranking.
// Comparing against the pool minimum rather than an absolute level means a
// uniformly busy pool keeps its warm picks — the cap only bites when
// stickiness has made a replica an outlier. When the entire warm set is over
// the cap the pick falls back to the least-loaded candidate overall (within
// the cap by definition) and demoted reports the sacrificed warm pick.
// A negative delta disables the cap.
func cappedPick(ranked []Candidate, r, delta int) (host string, demoted bool) {
	if delta < 0 {
		return leastLoaded(ranked[:r]).Host, false
	}
	limit := minInFlight(ranked) + delta
	best := -1
	for i, c := range ranked[:r] {
		if c.InFlight <= limit && (best < 0 || c.InFlight < ranked[best].InFlight) {
			best = i
		}
	}
	if best >= 0 {
		return ranked[best].Host, false
	}
	return leastLoaded(ranked).Host, true
}
