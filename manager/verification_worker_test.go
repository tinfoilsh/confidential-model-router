package manager

import (
	"context"
	"sync"
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
		{name: "above renewal window", configured: 2 * attestationRenewalWindow, want: attestationRefreshInterval},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				em := &EnclaveManager{
					models:          &sync.Map{},
					updateConfigURL: t.TempDir() + "/missing-config.yml",
					refreshInterval: test.configured,
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := time.Now()
				go em.runWorker(ctx)
				for range 3 {
					synctest.Wait()
					em.stateMu.Lock()
					got := em.lastAttemptedUpdate
					em.stateMu.Unlock()
					if !got.Equal(want) {
						t.Fatalf("last refresh = %v, want %v", got, want)
					}
					want = want.Add(test.want)
					time.Sleep(test.want)
				}
				cancel()
				synctest.Wait()
			})
		})
	}
}
