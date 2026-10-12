package manager

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestVerificationWorkerBoundsRefreshInterval(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "short interval", configured: time.Minute, want: time.Minute},
		{name: "above hourly", configured: 2 * attestationRefreshInterval, want: attestationRefreshInterval},
		{name: "above renewal window", configured: 2 * attestationRenewalWindow, want: attestationRefreshInterval},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts atomic.Int32
				originalClient := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					attempts.Add(1)
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Body:       io.NopCloser(strings.NewReader("config unavailable")),
					}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = originalClient })
				em := &EnclaveManager{
					models:          &sync.Map{},
					updateConfigURL: "https://config.example/router.yml",
					refreshInterval: test.configured,
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go em.runWorker(ctx)
				synctest.Wait()
				if got := attempts.Load(); got != 1 {
					t.Fatalf("initial refresh attempts = %d, want 1", got)
				}

				for wantAttempts := int32(2); wantAttempts <= 3; wantAttempts++ {
					time.Sleep(test.want)
					synctest.Wait()
					if got := attempts.Load(); got != wantAttempts {
						t.Fatalf("refresh attempts = %d, want %d after %v", got, wantAttempts, test.want)
					}
				}
				cancel()
				synctest.Wait()
			})
		})
	}
}
