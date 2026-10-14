package manager

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-model-router/config"
)

// vLLM updates its queue-depth gauge every engine step (tens of ms), so a
// 1s poll reads fresh data each time. A scrape that outlives the interval
// blocks the loop and the ticker coalesces missed ticks, so a slow backend
// is polled at scrape-duration cadence rather than piling up requests.
const defaultMetricsPollInterval = time.Second

const (
	waitingMetricName = "vllm:num_requests_waiting"
	runningMetricName = "vllm:num_requests_running"
)

type requestCounts struct {
	waiting, running       float64
	hasWaiting, hasRunning bool
}

func (s requestCounts) supports(cfg *config.OverloadConfig) bool {
	return cfg.Enabled() &&
		(cfg.MaxRequestsWaiting <= 0 || s.hasWaiting) &&
		(cfg.MaxRequestsRunning <= 0 || s.hasRunning)
}

// pollAPIKey, when set, is sent as a bearer token on backend /metrics
// scrapes (enclaves require an admin key). Stored atomically so startup
// wiring can't race a running poller.
var pollAPIKey atomic.Value // string

// SetMetricsPollAPIKey installs the credential for backend /metrics polls.
func SetMetricsPollAPIKey(key string) {
	pollAPIKey.Store(key)
}

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
	latestCounts    requestCounts
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

	// A missing block and all non-positive trip marks mean "no overload
	// evaluation". The reset must cover both: leaving a previously tripped
	// flag in place with evaluation stopped would de-prefer the enclave in
	// NextEnclave forever.
	if !cfg.Enabled() {
		m.overloaded.Store(false)
		m.updateLatest(requestCounts{}, time.Time{})
		BackendQueueDepth.WithLabelValues(m.model, m.host).Set(0)
		BackendRunningRequests.WithLabelValues(m.model, m.host).Set(0)
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

	// The flag may have been computed under the previous marks. If a fresh
	// sample exists, re-evaluate it against the new marks now — before the
	// poll goroutine starts, so there is no concurrent writer — rather than
	// leaving the old decision driving shouldReject until the first scrape
	// lands (which can take a while if the backend is unreachable). For an
	// unchanged config the evaluation is a fixed point, so the per-sync
	// config reinstall does not disturb hysteresis state.
	if counts, collected := m.latestSample(); !counts.supports(cfg) {
		m.overloaded.Store(false)
		BackendOverloaded.WithLabelValues(m.model, m.host).Set(0)
	} else if !collected.IsZero() && time.Since(collected) <= sampleStalenessLimit {
		m.evaluateThresholds(counts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	log.WithFields(log.Fields{
		"model":                  m.model,
		"enclave":                m.host,
		"max_requests_waiting":   trip,
		"clear_requests_waiting": clear,
		"max_requests_running":   cfg.MaxRequestsRunning,
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
	if key, _ := pollAPIKey.Load().(string); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
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
	if resp.StatusCode != http.StatusOK {
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
			"status":  resp.StatusCode,
		}).Debug("metrics poll returned non-OK status")
		return
	}

	counts, err := extractRequestCounts(resp.Body)
	if err != nil {
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Debugf("metrics parse failed: %v", err)
		return
	}
	if !counts.supports(m.currentConfig()) {
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Debug("metrics sample missing an enabled overload metric")
		return
	}

	log.WithFields(log.Fields{
		"model":            m.model,
		"enclave":          m.host,
		"requests_waiting": counts.waiting,
		"requests_running": counts.running,
	}).Debug("polled vLLM metrics")

	if counts.hasWaiting {
		BackendQueueDepth.WithLabelValues(m.model, m.host).Set(counts.waiting)
	} else {
		BackendQueueDepth.DeleteLabelValues(m.model, m.host)
	}
	if counts.hasRunning {
		BackendRunningRequests.WithLabelValues(m.model, m.host).Set(counts.running)
	} else {
		BackendRunningRequests.DeleteLabelValues(m.model, m.host)
	}

	m.updateLatest(counts, time.Now())
	m.evaluateThresholds(counts)
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

func extractRequestCounts(r io.Reader) (requestCounts, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return requestCounts{}, err
	}
	var counts requestCounts
	for _, metric := range []struct {
		name    string
		value   *float64
		present *bool
	}{
		{waitingMetricName, &counts.waiting, &counts.hasWaiting},
		{runningMetricName, &counts.running, &counts.hasRunning},
	} {
		for _, sample := range families[metric.name].GetMetric() {
			var value float64
			switch {
			case sample.Gauge != nil:
				value = sample.Gauge.GetValue()
			case sample.Untyped != nil:
				value = sample.Untyped.GetValue()
			default:
				return requestCounts{}, fmt.Errorf("metric %s is not a gauge", metric.name)
			}
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return requestCounts{}, fmt.Errorf("invalid request count for %s", metric.name)
			}
			*metric.value += value
			*metric.present = true
		}
	}
	return counts, nil
}

func (m *enclaveMetrics) currentConfig() *config.OverloadConfig {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg
}

func (m *enclaveMetrics) updateLatest(counts requestCounts, ts time.Time) {
	m.latestMu.Lock()
	m.latestCounts = counts
	m.latestCollected = ts
	m.latestMu.Unlock()
}

func (m *enclaveMetrics) latestSample() (requestCounts, time.Time) {
	m.latestMu.RLock()
	defer m.latestMu.RUnlock()
	return m.latestCounts, m.latestCollected
}

// sampleStalenessLimit bounds how long the overload flag keeps driving
// shouldReject without a fresh sample. It is a fixed window rather than a
// multiple of the poll interval: at a 1s cadence, deriving it from the
// interval would fail open after a single slow scrape (the scrape client
// timeout alone is 5s). 15s means roughly three consecutive timed-out
// scrapes must elapse before the safety net is dropped.
const sampleStalenessLimit = 15 * time.Second

// evaluateThresholds combines a running-request limit with queue hysteresis.
// Running load clears below its limit; a tripped overload flag also waits for
// the queue to reach its clear mark before allowing new requests.
func (m *enclaveMetrics) evaluateThresholds(counts requestCounts) {
	cfg := m.currentConfig()
	if !counts.supports(cfg) {
		return
	}

	trip, clear := cfg.Marks()
	runningTrip := cfg.MaxRequestsRunning
	wasOverloaded := m.overloaded.Load()
	overloaded := wasOverloaded
	switch {
	case (trip > 0 && counts.waiting >= float64(trip)) || (runningTrip > 0 && counts.running >= float64(runningTrip)):
		overloaded = true
	case (trip <= 0 || counts.waiting <= float64(clear)) && (runningTrip <= 0 || counts.running < float64(runningTrip)):
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
			"requests_waiting":       counts.waiting,
			"max_requests_waiting":   trip,
			"clear_requests_waiting": clear,
			"requests_running":       counts.running,
			"max_requests_running":   runningTrip,
			"retry_after_minutes":    cfg.RetryAfterMinutes,
		}).Warn("overload threshold exceeded")
		OverloadEventsTotal.WithLabelValues(m.model, m.host).Inc()
	case !overloaded && wasOverloaded:
		log.WithFields(log.Fields{
			"model":                  m.model,
			"enclave":                m.host,
			"requests_waiting":       counts.waiting,
			"clear_requests_waiting": clear,
			"requests_running":       counts.running,
			"max_requests_running":   runningTrip,
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
// running load falls below its limit and the queue reaches its clear mark.
// A missing or stale sample fails open.
func (m *enclaveMetrics) shouldReject() (bool, time.Duration, float64) {
	cfg := m.currentConfig()
	if !cfg.Enabled() {
		return false, 0, 0
	}

	counts, collected := m.latestSample()
	waiting := counts.waiting
	if collected.IsZero() || !counts.supports(cfg) {
		return false, 0, waiting
	}
	if time.Since(collected) > sampleStalenessLimit {
		OverloadFailOpenTotal.WithLabelValues(m.model, m.host).Inc()
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
