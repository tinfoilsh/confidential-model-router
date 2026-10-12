package manager

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// blackholeListener returns the address of a TCP listener that accepts
// connections at the kernel level but never completes a TLS handshake —
// standing in for a host whose inbound packets are being dropped.
func blackholeListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func shortVerifyTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := enclaveVerifyTimeout
	enclaveVerifyTimeout = d
	t.Cleanup(func() { enclaveVerifyTimeout = old })
}

// Regression test to avoid  model-wide routing freeze: while addEnclave is
// blocked on network I/O to an unresponsive host, the model lock must remain
// available to request routing.
func TestAddEnclaveNetworkIODoesNotHoldModelLock(t *testing.T) {
	started := make(chan struct{})
	blocked := make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	var first sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Do(func() { close(started) })
		<-blocked
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	trustVerificationServer(t, server)

	model := &Model{Repo: renewalTestRepo, Enclaves: map[string]*Enclave{}}
	em := &EnclaveManager{models: &sync.Map{}, verifier: verifyEnclave}
	em.models.Store("test-model", model)

	done := make(chan struct{})
	var verificationError error
	go func() {
		defer close(done)
		verificationError = em.addEnclave("test-model", strings.TrimPrefix(server.URL, "https://"))
	}()
	t.Cleanup(func() { release(); <-done })

	// Let addEnclave reach its network call, then verify the write lock —
	// the strictest form of "routing can proceed" — is acquirable.
	waitRenewalSignal(t, started)
	locked := make(chan struct{})
	go func() {
		model.mu.Lock()
		if len(model.Enclaves) != 0 {
			t.Error("enclave inserted before verification completed")
		}
		model.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(renewalTestTimeout):
		t.Fatal("model lock held while addEnclave was blocked on network I/O")
	}

	release()
	waitRenewalSignal(t, done)
	if verificationError == nil || !strings.Contains(verificationError.Error(), "attestation") {
		t.Fatalf("expected attestation fetch error, got %v", verificationError)
	}
}
