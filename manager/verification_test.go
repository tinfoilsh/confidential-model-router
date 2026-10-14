package manager

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go/enclave"
)

func trustVerificationServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		http.DefaultTransport = original
	})
}

func TestAddEnclaveRejectsUnverifiedDocument(t *testing.T) {
	var challenges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenges.Add(1)
		io.WriteString(w, `{"format":"https://tinfoil.sh/predicate/attestation/v3"}`)
	}))
	defer server.Close()
	trustVerificationServer(t, server)
	key, err := enclave.CertPubkeyFP(server.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(server.URL, "https://")
	for _, name := range []string{"new", "expired"} {
		t.Run(name, func(t *testing.T) {
			model := &Model{Repo: renewalTestRepo, ConfigRef: registryTestRef, Enclaves: map[string]*Enclave{}}
			if name == "expired" {
				model.Enclaves[host] = &Enclave{tlsKeyFP: key, attestation: newAttestationState(verifiedAttestation{expiresAt: time.Now().Add(-time.Second), verifiedAt: time.Now()})}
			}
			previous := model.Enclaves[host]
			attempts := challenges.Load()
			em := &EnclaveManager{models: &sync.Map{}, verifier: verifyEnclave, refreshInterval: time.Minute}
			em.models.Store(renewalTestModel, model)
			if err := em.addEnclave(renewalTestModel, host); err == nil || !strings.Contains(err.Error(), "failed to verify remote attestation") {
				t.Fatalf("expected verifier rejection, got %v", err)
			}
			if challenges.Load() == attempts || model.Enclaves[host] != previous || model.SourceMeasurement != nil || model.Tag != "" {
				t.Fatal("unverified document was accepted or expired evidence skipped verification")
			}
		})
	}
}

func TestAttestedTransportEnforcesKeyAndExpiration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, "verified response")
	}))
	defer server.Close()
	trustVerificationServer(t, server)
	key, err := enclave.CertPubkeyFP(server.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	transport := newAttestedTransport(key, newAttestationState(verifiedAttestation{expiresAt: time.Now().Add(time.Hour), verifiedAt: time.Now()}))
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "verified response" {
		t.Fatalf("response = %q, %v", body, err)
	}

	transport.attestation.current.Store(&verifiedAttestation{expiresAt: time.Now().Add(-time.Second)})
	closed := &trackedVerificationBody{Reader: bytes.NewReader([]byte("private request"))}
	request, _ = http.NewRequest(http.MethodPost, server.URL, closed)
	_, err = transport.RoundTrip(request)
	var attestationErr *enclave.AttestationError
	if !errors.As(err, &attestationErr) || !closed.closed || calls.Load() != 1 {
		t.Fatalf("expired request: err=%v closed=%v upstream calls=%d", err, closed.closed, calls.Load())
	}

	if got := classifyProxyError(err); got != "attestation_expired" {
		t.Fatalf("expired evidence classified as %q", got)
	}

	wrongKey := newAttestedTransport(strings.Repeat("0", len(key)), newAttestationState(verifiedAttestation{expiresAt: time.Now().Add(time.Hour), verifiedAt: time.Now()}))
	defer wrongKey.CloseIdleConnections()
	request, _ = http.NewRequest(http.MethodGet, server.URL, nil)
	_, err = wrongKey.RoundTrip(request)
	if !errors.As(err, &attestationErr) || calls.Load() != 1 {
		t.Fatalf("wrong-key request: err=%v upstream calls=%d", err, calls.Load())
	}
	if got := classifyProxyError(err); got != "tls_mismatch" {
		t.Fatalf("wrong key classified as %q", got)
	}
}

type trackedVerificationBody struct {
	*bytes.Reader
	closed bool
}

func (b *trackedVerificationBody) Close() error { b.closed = true; return nil }

func TestTLSAddress(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"enclave.example", "enclave.example:443"},
		{"enclave.example:8443", "enclave.example:8443"},
		{"127.0.0.1", "127.0.0.1:443"},
		{"[::1]", "[::1]:443"},
		{"[::1]:8443", "[::1]:8443"},
		{"::1", "[::1]:443"},
		{"[fe80::1%eth0]", "[fe80::1%eth0]:443"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := tlsAddress(tc.host); got != tc.want {
				t.Fatalf("tlsAddress(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}
