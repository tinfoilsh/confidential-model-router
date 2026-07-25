package manager

import (
	"net"
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
	shortVerifyTimeout(t, 2*time.Second)
	addr := blackholeListener(t)

	model := &Model{Enclaves: map[string]*Enclave{}}
	em := &EnclaveManager{models: &sync.Map{}}
	em.models.Store("test-model", model)

	done := make(chan error, 1)
	go func() { done <- em.addEnclave("test-model", addr, nil) }()

	// Let addEnclave reach its network call, then verify the write lock —
	// the strictest form of "routing can proceed" — is acquirable.
	time.Sleep(100 * time.Millisecond)
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
	case <-time.After(500 * time.Millisecond):
		t.Fatal("model lock held while addEnclave was blocked on network I/O")
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "attestation") {
			t.Fatalf("expected attestation fetch error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("addEnclave did not return after verify timeout")
	}
}
