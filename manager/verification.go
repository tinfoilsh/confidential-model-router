package manager

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tinfoilsh/tinfoil-go/enclave"
	"github.com/tinfoilsh/tinfoil-go/verify"
)

const (
	maxAttestationBytes        = 32 << 20
	attestationRenewalWindow   = 24 * time.Hour
	attestationRefreshInterval = time.Hour
	runtimeConfigTimeout       = 30 * time.Second
)

type attestationVerifier interface {
	VerifyV3([]byte, []byte, string) (*verify.Verification, error)
}

type verifiedAttestation struct {
	expiresAt  time.Time
	verifiedAt time.Time
	codeTag    string
}

// Each state belongs to one verified key pair. Renewals can update existing
// transports, while a key change leaves old transports bound to the old state.
type attestationState struct {
	current atomic.Pointer[verifiedAttestation]
}

func newAttestationState(expiresAt, verifiedAt time.Time, codeTag string) *attestationState {
	s := &attestationState{}
	s.current.Store(&verifiedAttestation{expiresAt: expiresAt, verifiedAt: verifiedAt, codeTag: codeTag})
	return s
}

func (s *attestationState) load() *verifiedAttestation {
	if s == nil {
		return nil
	}
	return s.current.Load()
}

func (e *Enclave) attestationValid(now time.Time) bool {
	snapshot := e.attestation.load()
	return snapshot == nil || now.Before(snapshot.expiresAt)
}

func (e *Enclave) attestationRefreshDue(now time.Time, refreshInterval time.Duration) bool {
	snapshot := e.attestation.load()
	if snapshot == nil {
		return false
	}
	return !now.Add(max(attestationRenewalWindow, refreshInterval)).Before(snapshot.expiresAt) ||
		!now.Before(snapshot.verifiedAt.Add(attestationRefreshInterval))
}

var errAttestationExpired = errors.New("enclave attestation has expired")

func tlsAddress(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), "443")
}

// attestedTransport rejects new requests once the authenticated witness expires,
// including requests on pooled connections and caller-managed MCP sessions.
type attestedTransport struct {
	*enclave.TLSBoundRoundTripper
	attestation *attestationState
}

func newAttestedTransport(key string, attestation *attestationState) *attestedTransport {
	return &attestedTransport{
		TLSBoundRoundTripper: &enclave.TLSBoundRoundTripper{ExpectedPublicKey: key},
		attestation:          attestation,
	}
}

func (t *attestedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if snapshot := t.attestation.load(); snapshot != nil && !time.Now().Before(snapshot.expiresAt) {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, &enclave.AttestationError{Err: errAttestationExpired}
	}
	return t.TLSBoundRoundTripper.RoundTrip(req)
}
