package manager

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go/document"
	"github.com/tinfoilsh/tinfoil-go/enclave"
	"github.com/tinfoilsh/tinfoil-go/verify"
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

func TestAttestationFetchChallengeAndBounds(t *testing.T) {
	nonce, err := document.RandomNonce()
	if err != nil {
		t.Fatal(err)
	}
	const payload = `{"format":"invalid"}`
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"ok", http.StatusOK, payload, false},
		{"error", http.StatusServiceUnavailable, payload, true},
		{"redirect", http.StatusTemporaryRedirect, payload, true},
		{"oversized", http.StatusOK, strings.Repeat("x", maxAttestationBytes+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/tinfoil-attestation" || r.URL.Query().Get("nonce") != hex.EncodeToString(nonce) {
					t.Errorf("incorrect attestation challenge: %s", r.URL)
				}
				w.Header().Set("Location", "http://127.0.0.1/forbidden")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			trustVerificationServer(t, server)
			body, err := attestationFetch(strings.TrimPrefix(server.URL, "https://"), nonce)
			if (err != nil) != tc.wantError {
				t.Fatalf("fetch error = %v, want error %v", err, tc.wantError)
			}
			if !tc.wantError && string(body) != payload {
				t.Fatalf("unexpected document: %s", body)
			}
		})
	}
}

func TestAddEnclaveRejectsUnverifiedDocument(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"format":"https://tinfoil.sh/predicate/attestation/v3"}`)
	}))
	defer server.Close()
	trustVerificationServer(t, server)
	verifier, err := verify.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	model := &Model{Repo: "tinfoilsh/confidential-gpt-oss-120b", Enclaves: map[string]*Enclave{}}
	em := &EnclaveManager{models: &sync.Map{}, verifier: verifier}
	em.models.Store("gpt-oss-120b", model)
	err = em.addEnclave("gpt-oss-120b", strings.TrimPrefix(server.URL, "https://"))
	if err == nil || !strings.Contains(err.Error(), "failed to verify remote attestation") {
		t.Fatalf("expected verifier rejection, got %v", err)
	}
	if len(model.Enclaves) != 0 || model.SourceMeasurement != nil || model.Tag != "" {
		t.Fatal("unverified document modified the model")
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
	transport := newAttestedTransport(key, time.Now().Add(time.Hour))
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

	transport.expiresAt = time.Now().Add(-time.Second)
	closed := &trackedVerificationBody{Reader: bytes.NewReader([]byte("private request"))}
	request, _ = http.NewRequest(http.MethodPost, server.URL, closed)
	_, err = transport.RoundTrip(request)
	var attestationErr *enclave.AttestationError
	if !errors.As(err, &attestationErr) || !closed.closed || calls.Load() != 1 {
		t.Fatalf("expired request: err=%v closed=%v upstream calls=%d", err, closed.closed, calls.Load())
	}

	wrongKey := newAttestedTransport(strings.Repeat("0", len(key)), time.Now().Add(time.Hour))
	defer wrongKey.CloseIdleConnections()
	request, _ = http.NewRequest(http.MethodGet, server.URL, nil)
	_, err = wrongKey.RoundTrip(request)
	if !errors.As(err, &attestationErr) || calls.Load() != 1 {
		t.Fatalf("wrong-key request: err=%v upstream calls=%d", err, calls.Load())
	}
}

type trackedVerificationBody struct {
	*bytes.Reader
	closed bool
}

func (b *trackedVerificationBody) Close() error { b.closed = true; return nil }
