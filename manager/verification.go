package manager

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tinfoilsh/tinfoil-go/enclave"
)

const maxAttestationBytes = 32 << 20

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
	expiresAt time.Time
}

func newAttestedTransport(key string, expiresAt time.Time) *attestedTransport {
	return &attestedTransport{
		TLSBoundRoundTripper: &enclave.TLSBoundRoundTripper{ExpectedPublicKey: key},
		expiresAt:            expiresAt,
	}
}

func (t *attestedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.expiresAt.IsZero() && !time.Now().Before(t.expiresAt) {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, &enclave.AttestationError{Err: errAttestationExpired}
	}
	return t.TLSBoundRoundTripper.RoundTrip(req)
}
