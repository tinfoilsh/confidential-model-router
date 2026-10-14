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
	return cfg.Enabled() && s.hasWaiting &&
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

	stateMu         sync.RWMutex
	nativePolicy    *config.OverloadConfig
	requiresRunning bool
	latestCounts    requestCounts
	latestCollected time.Time
	queueOverloads  map[queueMarks]bool
}

type queueMarks struct{ trip, clear int }

func (q queueMarks) overloaded(waiting float64, previous bool) bool {
	if waiting >= float64(q.trip) {
		return true
	}
	return previous && waiting > float64(q.clear)
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

func (m *enclaveMetrics) setPolicies(native *config.OverloadConfig, policies []*config.OverloadConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.wg.Wait()
		m.cancel = nil
	}
	if !m.configurePolicies(native, policies) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	log.WithFields(log.Fields{"model": m.model, "enclave": m.host}).Info("starting vLLM metrics polling")
	go m.run(ctx, defaultMetricsPollInterval)
}

// configurePolicies retains recovery state only for configured queue marks.
// The native policy selects the backend's telemetry; routing uses the caller's
// policy against this same state, including when the caller spills to shared.
func (m *enclaveMetrics) configurePolicies(native *config.OverloadConfig, policies []*config.OverloadConfig) bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	wasOverloaded := m.queueOverloadedLocked(m.nativePolicy)
	m.nativePolicy = native
	m.requiresRunning = false
	states := make(map[queueMarks]bool)
	poll := false
	for _, policy := range append([]*config.OverloadConfig{native}, policies...) {
		if !policy.Enabled() {
			continue
		}
		poll = true
		m.requiresRunning = m.requiresRunning || policy.MaxRequestsRunning > 0
		trip, clear := policy.Marks()
		if trip > 0 {
			marks := queueMarks{trip, clear}
			state := m.queueOverloads[marks]
			if m.latestCounts.hasWaiting && !m.latestCollected.IsZero() && time.Since(m.latestCollected) <= sampleStalenessLimit {
				state = marks.overloaded(m.latestCounts.waiting, state)
			}
			states[marks] = state
		}
	}
	m.queueOverloads = states
	if !poll {
		m.latestCounts = requestCounts{}
		m.latestCollected = time.Time{}
		BackendQueueDepth.DeleteLabelValues(m.model, m.host)
		BackendRunningRequests.DeleteLabelValues(m.model, m.host)
	}
	m.publishOverloadLocked(wasOverloaded)
	return poll
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
	if !m.supportsSample(counts) {
		log.WithFields(log.Fields{
			"model":   m.model,
			"enclave": m.host,
		}).Debug("metrics sample missing an enabled overload metric")
		return
	}

	m.updateLatest(counts, time.Now())
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

func (m *enclaveMetrics) supportsSample(counts requestCounts) bool {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return counts.hasWaiting && (!m.requiresRunning || counts.hasRunning)
}

func (m *enclaveMetrics) updateLatest(counts requestCounts, ts time.Time) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	wasOverloaded := m.queueOverloadedLocked(m.nativePolicy)
	m.latestCounts = counts
	m.latestCollected = ts
	if counts.hasWaiting {
		for marks, state := range m.queueOverloads {
			m.queueOverloads[marks] = marks.overloaded(counts.waiting, state)
		}
	}
	m.publishOverloadLocked(wasOverloaded)
}

func (m *enclaveMetrics) latestSample() (requestCounts, time.Time) {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.latestCounts, m.latestCollected
}

func (m *enclaveMetrics) queueOverloadedLocked(policy *config.OverloadConfig) bool {
	if policy == nil || policy.MaxRequestsWaiting <= 0 {
		return false
	}
	trip, clear := policy.Marks()
	return m.queueOverloads[queueMarks{trip, clear}]
}

// sampleStalenessLimit bounds how long a load sample drives routing. It is a
// fixed window rather than a multiple of the poll interval: at a 1s cadence, deriving it from the
// interval would fail open after a single slow scrape (the scrape client
// timeout alone is 5s). 15s means roughly three consecutive timed-out
// scrapes must elapse before the safety net is dropped.
const sampleStalenessLimit = 15 * time.Second

func (m *enclaveMetrics) publishOverloadLocked(wasOverloaded bool) {
	overloaded := m.queueOverloadedLocked(m.nativePolicy)
	fields := log.Fields{
		"model":            m.model,
		"enclave":          m.host,
		"requests_waiting": m.latestCounts.waiting,
		"requests_running": m.latestCounts.running,
	}
	if m.nativePolicy != nil {
		trip, clear := m.nativePolicy.Marks()
		fields["max_requests_waiting"] = trip
		fields["clear_requests_waiting"] = clear
	}
	// The two message strings below predate hysteresis and are load-bearing:
	// external log-based alerts may key on them, so keep them stable and
	// evolve the structured fields instead.
	switch {
	case overloaded && !wasOverloaded:
		log.WithFields(fields).Warn("overload threshold exceeded")
		OverloadEventsTotal.WithLabelValues(m.model, m.host).Inc()
	case !overloaded && wasOverloaded:
		log.WithFields(fields).Info("queue depth back below overload threshold")
		RecoveryEventsTotal.WithLabelValues(m.model, m.host).Inc()
	}

	if overloaded {
		BackendOverloaded.WithLabelValues(m.model, m.host).Set(1)
	} else {
		BackendOverloaded.WithLabelValues(m.model, m.host).Set(0)
	}
}
