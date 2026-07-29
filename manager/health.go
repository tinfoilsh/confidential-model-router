package manager

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

const (
	defaultHealthProbeInterval = 5 * time.Second
	healthProbeTimeout         = 30 * time.Second
)

// enclaveHealthProber measures whether the backend event loop can answer
// /health. It is deliberately observational: failures do not affect replica
// selection or circuit-breaker state.
type enclaveHealthProber struct {
	host  string
	model string

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup

	client *http.Client
}

func newEnclaveHealthProber(host, model, tlsKeyFP string) *enclaveHealthProber {
	return &enclaveHealthProber{
		host:  host,
		model: model,
		client: &http.Client{
			Timeout: healthProbeTimeout,
			Transport: &tinfoilClient.TLSBoundRoundTripper{
				ExpectedPublicKey: tlsKeyFP,
			},
		},
	}
}

func (p *enclaveHealthProber) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	go p.run(ctx)
}

func (p *enclaveHealthProber) run(ctx context.Context) {
	defer p.wg.Done()

	p.probe(ctx)

	ticker := time.NewTicker(defaultHealthProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probe(ctx)
		}
	}
}

func (p *enclaveHealthProber) probe(ctx context.Context) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("https://%s/health", p.host),
		nil,
	)
	if err != nil {
		log.WithFields(log.Fields{
			"model":   p.model,
			"enclave": p.host,
		}).Debugf("health probe request creation failed: %v", err)
		return
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	elapsed := time.Since(start).Seconds()
	if err != nil {
		// Shutdown cancellation is not a backend failure and should not leave
		// a misleading error observation.
		if ctx.Err() != nil {
			return
		}
		BackendHealthProbeDurationSeconds.WithLabelValues(p.model, p.host, "error").Observe(elapsed)
		log.WithFields(log.Fields{
			"model":   p.model,
			"enclave": p.host,
		}).Debugf("health probe failed after %.3fs: %v", elapsed, err)
		return
	}
	defer resp.Body.Close()

	result := "success"
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result = "error"
	}
	BackendHealthProbeDurationSeconds.WithLabelValues(p.model, p.host, result).Observe(elapsed)

	if result == "error" {
		log.WithFields(log.Fields{
			"model":   p.model,
			"enclave": p.host,
			"status":  resp.StatusCode,
		}).Debugf("health probe returned an error after %.3fs", elapsed)
	}
}

func (p *enclaveHealthProber) shutdown() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}
