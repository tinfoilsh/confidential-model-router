package manager

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/tinfoil-go/document"
	"github.com/tinfoilsh/tinfoil-go/verify"
)

// TINFOIL_VERIFIER_TEST_CONFIG opts into read-only verification of a live pool.
func TestLiveV3Verification(t *testing.T) {
	path := os.Getenv("TINFOIL_VERIFIER_TEST_CONFIG")
	if path == "" {
		t.Skip("set TINFOIL_VERIFIER_TEST_CONFIG to a live router config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := verify.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	checkedTypes := map[string]bool{}
	for model, entry := range cfg.Models {
		for _, host := range entry.Hostnames {
			t.Run(model+"/"+host, func(t *testing.T) {
				nonce, err := document.RandomNonce()
				if err != nil {
					t.Fatal(err)
				}
				body, err := attestationFetch(host, nonce)
				if err != nil {
					t.Fatal(err)
				}
				verified, err := verifier.VerifyV3(body, nonce, entry.Repo)
				if err != nil {
					t.Fatal(err)
				}
				key, err := verified.TLSPublicKeyFP()
				if err != nil {
					t.Fatal(err)
				}
				actual, err := tlsPublicKeyFP(host)
				if err != nil {
					t.Fatal(err)
				}
				if key != actual {
					t.Fatalf("attested TLS key does not match serving certificate")
				}
				var envelope struct {
					CollateralFormat string `json:"collateral_format"`
				}
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				kind := string(verified.EnclaveMeasurement.Type) + "/" + envelope.CollateralFormat
				t.Logf("verified %s, expires %s", kind, verified.FreshnessExpiresAt)
				if checkedTypes[kind] {
					return
				}
				checkedTypes[kind] = true
				otherNonce := append([]byte(nil), nonce...)
				otherNonce[0] ^= 1
				if _, err := verifier.VerifyV3(body, otherNonce, entry.Repo); err == nil {
					t.Fatal("accepted wrong nonce")
				}
				if _, err := verifier.VerifyV3(body, nonce, "tinfoilsh/wrong-workload"); err == nil {
					t.Fatal("accepted wrong workload")
				}
				var tampered map[string]json.RawMessage
				if err := json.Unmarshal(body, &tampered); err != nil {
					t.Fatal(err)
				}
				tampered["crypto_material"] = json.RawMessage(`"e30="`)
				badBody, err := json.Marshal(tampered)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := verifier.VerifyV3(badBody, nonce, entry.Repo); err == nil {
					t.Fatal("accepted substituted keys")
				}
			})
		}
	}
	if len(checkedTypes) == 0 {
		t.Fatal("config contained no enclaves")
	}
}
