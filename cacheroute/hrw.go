package cacheroute

import (
	"math"
	"slices"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// Candidate is one replica the selector could pick: its stable enclave
// hostname (the HRW identity — it must survive redeploys, §5.2) and its
// current router-local in-flight request count (the live load signal for
// splitting hot keys, §5.4/§5.5).
type Candidate struct {
	Host     string
	InFlight int
}

// Pool is a model's replica set as seen at selection time. Size is the
// configured replica count (a pool of one can't route anywhere).
// Candidates are the breaker-closed replicas — the stable HRW membership of
// §5.3 — falling back to all replicas when every breaker is open, mirroring
// the degraded fallback of the production selector and §5.5's select().
type Pool struct {
	Size       int
	Candidates []Candidate
}

// rank orders candidates by rendezvous (HRW) score for key, highest first:
//
//	score(replica) = xxhash64(routing_key ‖ replica.host)
//
// No framing is needed inside the score: the key is fixed-width, so the
// field boundary can't shift, and xxhash only has to spread an
// already-uniform input (§6). Ties — vanishingly rare with 64-bit scores —
// break to the lexicographically smaller host so every router breaks them
// identically. rank sorts a copy; the caller's slice is untouched.
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

// replicationFactor is R, the size of a key's warm set (§5.4): roughly one
// replica per replica's-worth of traffic, at least the home, at most the
// whole pool. Below the threshold R = 1 and nothing changes for normal keys.
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

// leastLoaded picks the candidate with the fewest in-flight requests,
// breaking ties toward the earlier (higher-ranked) entry so the pick is
// deterministic and favors the key's home. Callers pass the top R of an HRW
// ranking; with R = 1 this is simply the home.
func leastLoaded(ranked []Candidate) Candidate {
	best := ranked[0]
	for _, c := range ranked[1:] {
		if c.InFlight < best.InFlight {
			best = c
		}
	}
	return best
}
