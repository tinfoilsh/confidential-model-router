package manager

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tinfoilsh/tinfoil-go/enclave"
	"github.com/tinfoilsh/tinfoil-go/verify"
	"github.com/tinfoilsh/tinfoil-go/verify/measurement"
)

const (
	attestationRenewalWindow   = 24 * time.Hour
	attestationRefreshInterval = time.Hour
	runtimeConfigTimeout       = 30 * time.Second
)

func verifyEnclave(host, repo string) (*verify.Verification, error) {
	handle, err := enclave.NewHandle(host, repo, nil)
	if err != nil {
		return nil, err
	}
	return handle.Verify()
}

func (em *EnclaveManager) verifyBackend(host string, model *Model) (*verify.Verification, error) {
	var failures []error
	for _, anchor := range []struct {
		reference string
		registry  bool
	}{
		{reference: model.Repo},
		{reference: model.ConfigRef, registry: true},
	} {
		if anchor.reference == "" {
			continue
		}
		verified, err := em.verifier(host, anchor.reference)
		if err == nil {
			// The SDK selects the collateral flow from the document. A matching
			// name in the other authority must not satisfy this trust anchor.
			if anchor.registry {
				if verified.Config == nil || verified.Config.Source != verify.SourceEndorsement {
					err = fmt.Errorf("config_ref requires an endorsed registry config")
				}
			} else if verified.Config != nil {
				err = fmt.Errorf("repo requires repository-backed attestation")
			}
			if err == nil {
				return verified, nil
			}
		}
		failures = append(failures, fmt.Errorf("verifying %s: %w", anchor.reference, err))
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no backend trust anchors configured")
	}
	return nil, errors.Join(failures...)
}

type verifiedAttestation struct {
	expiresAt    time.Time
	verifiedAt   time.Time
	codeTag      string
	measurement  *measurement.Measurement
	configName   string
	configDigest string
}

// Each state belongs to one verified key pair. Renewals can update existing
// transports, while a key change leaves old transports bound to the old state.
type attestationState struct {
	current atomic.Pointer[verifiedAttestation]
}

func newAttestationState(snapshot verifiedAttestation) *attestationState {
	s := &attestationState{}
	s.current.Store(&snapshot)
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
		!now.Add(min(refreshInterval, attestationRefreshInterval)).Before(snapshot.verifiedAt.Add(attestationRefreshInterval))
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
