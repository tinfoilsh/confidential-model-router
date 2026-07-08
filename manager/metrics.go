package manager

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-model-router/config"
)

const defaultMetricsPollInterval = 15 * time.Second

// enclaveMetrics polls backend metrics and logs queue depth for observability.
type enclaveMetrics struct {
	host  string
	model string

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup

	client *http.Client

	cfgMu      sync.RWMutex
	cfg        *config.OverloadConfig
	overloaded atomic.Bool

	latestMu        sync.RWMutex
	latestWaiting   float64
	latestCollected time.Time
}

func newEnclaveMetrics(host, model string) *enclaveMetrics {
	return &enclaveMetrics{
		host:  host,
		model: model,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// setConfig starts or stops polling based on whether usable overload
// thresholds are provided.
func (m *enclaveMetrics) setConfig(cfg *config.OverloadConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
		m.wg.Wait()
		m.cancel = nil
	}

	m.cfgMu.Lock()
	m.cfg = cfg
	m.cfgMu.Unlock()

	// A missing block and a non-positive trip mark both mean "no overload
	// evaluation". The reset must cover both: leaving a previously tripped
	// flag in place with evaluation stopped would de-prefer the enclave in
	// NextEnclave forever.
	if cfg == nil || cfg.MaxRequestsWaiting <= 0 {
		m.overloaded.Store(false)
		m.updateLatest(0, time.Time{})
		BackendQueueDepth.WithLabelValues(m.model, m.host).Set(0)
		BackendOverloaded.WithLabelValues(m.model, m.host).Set(0)
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Debug("metrics polling disabled (no overload threshold)")
		return
	}

	trip, clear := cfg.Marks()
	if cfg.ClearRequestsWaiting != 0 && cfg.ClearRequestsWaiting != clear {
		log.WithFields(log.Fields{
			"model":                m.model,
			"enclave":              m.host,
			"configured_clear":     cfg.ClearRequestsWaiting,
			"resolved_clear":       clear,
			"max_requests_waiting": trip,
		}).Warn("clear_requests_waiting out of range, using default")
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	log.WithFields(log.Fields{
		"model":                  m.model,
		"enclave":                m.host,
		"max_requests_waiting":   trip,
		"clear_requests_waiting": clear,
		"retry_after_minutes":    cfg.RetryAfterMinutes,
	}).Info("starting vLLM metrics polling")
	go m.run(ctx, defaultMetricsPollInterval)
}

func (m *enclaveMetrics) run(ctx context.Context, interval time.Duration) {
	defer m.wg.Done()

	// Initial scrape, then on each tick.
	m.scrape(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.scrape(ctx)
		}
	}
}

func (m *enclaveMetrics) scrape(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s/metrics", m.host), nil)
	if err != nil {
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Debugf("metrics request creation failed: %v", err)
		return
	}

	resp, err := m.client.Do(req)
	if err != nil {
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Debugf("metrics poll failed: %v", err)
		return
	}
	defer resp.Body.Close()

	waiting, err := extractWaiting(resp.Body)
	if err != nil {
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Debugf("metrics parse failed: %v", err)
		return
	}

	log.WithFields(log.Fields{
		"model":            m.model,
		"enclave":          m.host,
		"requests_waiting": waiting,
	}).Debug("polled vLLM metrics")

	BackendQueueDepth.WithLabelValues(m.model, m.host).Set(waiting)

	m.updateLatest(waiting, time.Now())
	m.evaluateThresholds(waiting)
}

func (m *enclaveMetrics) shutdown() {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
		m.wg.Wait()
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Info("stopped vLLM metrics polling")
	}
}

func extractWaiting(r io.Reader) (float64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "vllm:num_requests_waiting") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("malformed metric line: %s", line)
			}
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				return 0, err
			}
			return value, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("metric vllm:num_requests_waiting not found")
}

func (m *enclaveMetrics) currentConfig() *config.OverloadConfig {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg
}

func (m *enclaveMetrics) updateLatest(waiting float64, ts time.Time) {
	m.latestMu.Lock()
	m.latestWaiting = waiting
	m.latestCollected = ts
	m.latestMu.Unlock()
}

func (m *enclaveMetrics) latestSample() (float64, time.Time) {
	m.latestMu.RLock()
	defer m.latestMu.RUnlock()
	return m.latestWaiting, m.latestCollected
}

const sampleStalenessLimit = 3 * defaultMetricsPollInterval

// evaluateThresholds updates the overload flag from the latest queue depth
// with hysteresis: trip at the high mark, clear at the lower mark, hold the
// previous state in between. Without the band, a queue hovering at a single
// threshold would flap the flag on every poll — and with it the spill and
// reject decisions that read the flag.
func (m *enclaveMetrics) evaluateThresholds(waiting float64) {
	cfg := m.currentConfig()
	if cfg == nil || cfg.MaxRequestsWaiting <= 0 {
		return
	}

	trip, clear := cfg.Marks()
	wasOverloaded := m.overloaded.Load()
	overloaded := wasOverloaded
	switch {
	case waiting >= float64(trip):
		overloaded = true
	case waiting <= float64(clear):
		overloaded = false
	}

	// The two message strings below predate hysteresis and are load-bearing:
	// external log-based alerts may key on them, so keep them stable and
	// evolve the structured fields instead.
	switch {
	case overloaded && !wasOverloaded:
		log.WithFields(log.Fields{
			"model":                  m.model,
			"enclave":                m.host,
			"requests_waiting":       waiting,
			"max_requests_waiting":   trip,
			"clear_requests_waiting": clear,
			"retry_after_minutes":    cfg.RetryAfterMinutes,
		}).Warn("overload threshold exceeded")
		OverloadEventsTotal.WithLabelValues(m.model, m.host).Inc()
	case !overloaded && wasOverloaded:
		log.WithFields(log.Fields{
			"model":                  m.model,
			"enclave":                m.host,
			"requests_waiting":       waiting,
			"clear_requests_waiting": clear,
		}).Info("queue depth back below overload threshold")
		RecoveryEventsTotal.WithLabelValues(m.model, m.host).Inc()
	}

	m.overloaded.Store(overloaded)
	if overloaded {
		BackendOverloaded.WithLabelValues(m.model, m.host).Set(1)
	} else {
		BackendOverloaded.WithLabelValues(m.model, m.host).Set(0)
	}
}

// shouldReject reports whether new requests to this enclave should be
// rejected, along with the Retry-After hint and the latest queue depth. It
// reads the hysteretic overload flag maintained by evaluateThresholds — the
// same state NextEnclave's preference uses — rather than re-deriving a
// single-threshold answer, so a tripped backend keeps shedding load until
// its queue has genuinely drained to the clear mark. A missing or stale
// sample fails open.
func (m *enclaveMetrics) shouldReject() (bool, time.Duration, float64) {
	cfg := m.currentConfig()
	if cfg == nil || cfg.MaxRequestsWaiting <= 0 {
		return false, 0, 0
	}

	waiting, collected := m.latestSample()
	if collected.IsZero() {
		return false, 0, waiting
	}
	if time.Since(collected) > sampleStalenessLimit {
		log.WithFields(log.Fields{
			"model":     m.model,
			"enclave":   m.host,
			"staleness": time.Since(collected),
		}).Debug("metrics sample stale, allowing request")
		return false, 0, waiting
	}

	if !m.overloaded.Load() {
		return false, 0, waiting
	}

	minutes := cfg.RetryAfterMinutes
	if minutes <= 0 {
		minutes = 1
	}
	retryAfter := time.Duration(minutes) * time.Minute
	return true, retryAfter, waiting
}
