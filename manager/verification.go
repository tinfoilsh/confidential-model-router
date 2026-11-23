package manager

import (
	"context"
	"net/http"
	"time"

	"github.com/tinfoilsh/tinfoil-go"
	configendorsement "github.com/tinfoilsh/tinfoil-go/endorsement/config"
	"github.com/tinfoilsh/tinfoil-go/verify"
	"github.com/tinfoilsh/tinfoil-go/verify/measurement"
)

const (
	attestationRenewalWindow   = 24 * time.Hour
	attestationRefreshInterval = time.Hour
	runtimeConfigTimeout       = 30 * time.Second
	// enclaveVerifyTimeout bounds one background verification.
	enclaveVerifyTimeout = 30 * time.Second
)

// enclaveClient is the part of the SDK's enclave client that the router uses.
// The client verifies the enclave against its pin, pins every connection to
// the attested TLS key, and verifies again by itself when the enclave rejects
// that key or the verification expires.
type enclaveClient interface {
	http.RoundTripper
	// current returns the verification that requests are sent under.
	current() attestation
	// reverify verifies again while the current verification is still valid.
	reverify(ctx context.Context) (attestation, error)
}

// attestation is what the router reads from a verification.
type attestation struct {
	verifiedAt      time.Time
	expiresAt       time.Time
	codeTag         string
	configRevision  string
	codeMeasurement measurement.Expected
	predicate       string
	tlsKeyFP        string
	hpkeKey         string
}

func (a attestation) refreshDue(now time.Time, refreshInterval time.Duration) bool {
	return !now.Add(max(attestationRenewalWindow, refreshInterval)).Before(a.expiresAt) ||
		!now.Add(min(refreshInterval, attestationRefreshInterval)).Before(a.verifiedAt.Add(attestationRefreshInterval))
}

type sdkClient struct {
	*tinfoil.EnclaveHTTPClient
}

// connectEnclave verifies host against pin before it returns.
func connectEnclave(ctx context.Context, host string, pin tinfoil.Pin) (enclaveClient, error) {
	client, err := tinfoil.NewEnclaveHTTPClient(ctx, host, pin, nil)
	if err != nil {
		return nil, err
	}
	return sdkClient{client}, nil
}

func (c sdkClient) RoundTrip(req *http.Request) (*http.Response, error) {
	return c.Transport.RoundTrip(req)
}

func (c sdkClient) current() attestation {
	return attestationOf(c.Current())
}

func (c sdkClient) reverify(ctx context.Context) (attestation, error) {
	verification, err := c.Reverify(ctx)
	if err != nil {
		return attestation{}, err
	}
	return attestationOf(verification), nil
}

// The predicate types the status endpoint reports for each platform.
const (
	sevSNPPredicate = "https://tinfoil.sh/predicate/sev-snp-guest/v2"
	tdxPredicate    = "https://tinfoil.sh/predicate/tdx-guest/v2"
)

func attestationOf(v *verify.Verification) attestation {
	tlsKeyFP, _ := v.TLSPublicKeyFP()
	hpkeKey, _ := v.HPKEPublicKey()
	registryConfig, _ := v.Config()
	var predicate string
	switch v.EnclaveMeasurement().(type) {
	case measurement.SEVSNP:
		predicate = sevSNPPredicate
	case measurement.TDX:
		predicate = tdxPredicate
	}
	return attestation{
		verifiedAt:      v.Metadata().VerifiedAt,
		expiresAt:       v.FreshnessExpiresAt(),
		codeTag:         v.CodeTag(),
		configRevision:  revisionOf(registryConfig),
		codeMeasurement: v.CodeMeasurement(),
		predicate:       predicate,
		tlsKeyFP:        tlsKeyFP,
		hpkeKey:         hpkeKey,
	}
}

// revisionOf returns the revision in the name of the registry config an
// enclave was verified against. It is empty for an enclave verified against a
// code release, which has no config.
func revisionOf(registryConfig verify.ConfigVerification) string {
	_, revision, err := configendorsement.ParseName(registryConfig.Name)
	if err != nil {
		return ""
	}
	return revision
}

// verified returns the verification the enclave's requests are sent under,
// read from the client so that a verification the client made by itself shows
// at once. It reports false for a local harness connection, which has none.
func (e *Enclave) verified() (attestation, bool) {
	if e.client == nil {
		return attestation{}, false
	}
	verified := e.client.current()
	return verified, !verified.expiresAt.IsZero()
}

// attestationValid reports whether requests can be sent to the enclave.
func (e *Enclave) attestationValid(now time.Time) bool {
	verified, ok := e.verified()
	return !ok || now.Before(verified.expiresAt)
}
