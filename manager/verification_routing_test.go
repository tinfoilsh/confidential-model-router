package manager

import (
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
)

func TestExpiredBackendsAreNeverSelected(t *testing.T) {
	model := newTestModel("expired", "valid")
	expired := model.Enclaves["expired"]
	expired.attestation = newAttestationState(verifiedAttestation{expiresAt: time.Now().Add(-time.Second), verifiedAt: time.Now(), codeTag: "expired-release"})
	valid := model.Enclaves["valid"]
	valid.attestation = newAttestationState(verifiedAttestation{expiresAt: time.Now().Add(time.Hour), verifiedAt: time.Now(), codeTag: "valid-release"})
	if model.HasHealthyEnclave(map[string]bool{"expired": true}, nil) {
		t.Fatal("healthy backend outside the caller's pool masked expired evidence")
	}
	model.applyReservations("gpt-oss-120b", []config.ReservationConfig{{OrgIDs: []string{"test-org"}, Enclaves: []string{"expired"}}}, []string{"expired", "valid"})

	for _, open := range []bool{false, true} {
		if open {
			expired.cb.storeState(cbOpen)
			expired.cb.lastFailureNano.Store(time.Now().Add(-cbCooldown - time.Second).UnixNano())
		}
		selected, claim := model.NextEnclave(nil)
		if selected != valid || claim != nil {
			t.Fatalf("random selection chose expired evidence: selected=%v claim=%v", selected, claim)
		}
		selected, claim = model.NextEnclavePreferring([]string{"expired", "valid"}, nil)
		if selected != valid || claim != nil {
			t.Fatalf("preferred selection chose expired evidence: selected=%v claim=%v", selected, claim)
		}
		primary, spill := model.ReservationPools("test-org")
		selected, claim = model.selectForDispatchPools([]string{"expired"}, true, primary, spill)
		if selected != valid || claim != nil {
			t.Fatalf("reserved selection failed to spill past expired evidence: selected=%v claim=%v", selected, claim)
		}
		if pool := model.CacheRoutePool(); len(pool.Candidates) != 1 || pool.Candidates[0].Host != valid.host {
			t.Fatalf("cache routing includes expired evidence: %+v", pool)
		}
	}

	valid.attestation.current.Store(&verifiedAttestation{expiresAt: time.Now().Add(-time.Second)})
	if model.HasHealthyEnclave(nil, nil) {
		t.Fatal("expired backend reported healthy")
	}
	if selected, claim := model.NextEnclavePreferring([]string{"expired", "valid"}, nil); selected != nil || claim != nil {
		t.Fatal("degraded fallback selected expired evidence")
	}
	if pool := model.CacheRoutePool(); len(pool.Candidates) != 0 {
		t.Fatal("cache routing fallback selected expired evidence")
	}
}
