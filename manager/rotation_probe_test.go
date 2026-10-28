package manager

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/tinfoil-go/enclave"
	"github.com/tinfoilsh/tinfoil-go/verify"
)

const periodicProbeTestChild = "TINFOIL_PERIODIC_PROBE_TEST_CHILD"

func TestRotationPeriodicProbeUsesCurrentVerification(t *testing.T) {
	// System certificate roots are cached process-wide. Isolate the fixture CA
	// so the production TLS probe can validate it without changing other tests.
	if os.Getenv(periodicProbeTestChild) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^"+t.Name()+"$")
		cmd.Env = append(os.Environ(), periodicProbeTestChild+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated probe test: %v\n%s", err, output)
		}
		return
	}

	for _, scenario := range []string{"fresh", "renewal due", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			var handshakes, verifications atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, renewalTestResponse)
			}))
			server.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				if handshakes.Add(1) == 1 {
					close(started)
					<-release
				}
				return nil, nil
			}}
			server.StartTLS()
			t.Cleanup(server.Close)
			trustVerificationServer(t, server)
			certPath := filepath.Join(t.TempDir(), "roots.pem")
			if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSL_CERT_FILE", certPath)
			t.Setenv("SSL_CERT_DIR", t.TempDir())
			key, err := enclave.CertPubkeyFP(server.Certificate())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			em, model, previous := renewalManager(t, server, now.Add(2*attestationRenewalWindow), now, func(string, string) (*verify.Verification, error) {
				verifications.Add(1)
				return renewalVerification(key, time.Now().Add(2*attestationRenewalWindow)), nil
			})
			t.Cleanup(func() {
				for _, current := range model.Enclaves {
					current.shutdown()
				}
			})
			previous.tlsKeyFP = strings.Repeat("0", len(key))
			var probeErr error
			go func() {
				defer close(done)
				probeErr = em.addEnclave(renewalTestModel, previous.host)
			}()
			t.Cleanup(func() { unblock(); <-done })
			waitRenewalSignal(t, started)
			ctx, cancel := context.WithTimeout(t.Context(), renewalTestTimeout)
			defer cancel()
			transport, err := em.recoverEnclave(ctx, renewalTestModel, previous.host, previous.tlsKeyFP)
			if err != nil {
				t.Fatal(err)
			}
			requireRenewalResponse(t, transport, server.URL+"/response")
			if got := verifications.Load(); got != 1 {
				t.Fatalf("recovery verifications=%d, want 1", got)
			}
			current := model.Enclaves[previous.host]
			wantVerifications := int32(1)
			if scenario != "fresh" {
				expiresAt := time.Now().Add(time.Hour)
				if scenario == "expired" {
					expiresAt = time.Now().Add(-time.Second)
				}
				current.attestation.current.Store(newAttestationState(expiresAt, now, "v1.0.1").load())
				wantVerifications++
			}
			unblock()
			waitRenewalSignal(t, done)
			if probeErr != nil || verifications.Load() != wantVerifications {
				t.Fatalf("probe error=%v verifications=%d, want %d", probeErr, verifications.Load(), wantVerifications)
			}
			if current.attestationRefreshDue(time.Now(), em.refreshInterval) {
				t.Fatal("periodic probe left verification due for renewal")
			}
		})
	}
}
