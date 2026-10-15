package manager

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/expfmt"
	prommodel "github.com/prometheus/common/model"

	"github.com/tinfoilsh/confidential-model-router/config"
)

const (
	tierShadowPollTimeout     = 5 * time.Second
	shadowRunning             = "running"
	shadowWaiting             = "waiting"
	shadowScrapeError         = "error"
	shadowScrapeComplete      = "complete"
	shadowScrapePartial       = "partial"
	shadowRequestMetricPrefix = "vllm:num_requests_"
)

var (
	tierShadowBackendRequests = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "router_tier_shadow_backend_requests", Help: "Observed backend request counts used only by tier shadow evaluation",
	}, []string{"model", "enclave", "state"})
	tierShadowSampleTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "router_tier_shadow_sample_timestamp_seconds", Help: "Timestamp of the last parsed tier shadow metrics sample; completeness is reported by shadow decisions",
	}, []string{"model", "enclave"})
	tierShadowScrapes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "router_tier_shadow_scrapes_total", Help: "Tier shadow metrics scrapes by result",
	}, []string{"model", "result"})
)

type tierShadowSample struct {
	waiting, running       float64
	hasWaiting, hasRunning bool
	collected              time.Time
}

type tierShadowMetrics struct {
	host, model string
	client      *http.Client
	cancel      context.CancelFunc
	done        chan struct{}
	mu          sync.RWMutex
	sample      tierShadowSample
	queues      map[config.TierShadowConfig]bool
}

// The model lock serializes configuration and retirement. Shadow pollers
// have no references to the live overload state or its metric collectors.
func (m *Model) configureTierShadowLocked(e *Enclave) {
	policies := make(map[config.TierShadowConfig]bool)
	add := func(policy *config.TierShadowConfig) {
		if policy != nil {
			policies[*policy] = false
		}
	}
	if m.reservedByOrg == nil || m.sharedHosts[e.host] {
		add(m.TierShadow)
	}
	for org, hosts := range m.reservedByOrg {
		if hosts[e.host] || m.sharedHosts[e.host] {
			add(m.shadowPoliciesByOrg[org])
		}
	}
	if len(policies) == 0 {
		if e.tierShadow != nil {
			e.tierShadow.stop()
			e.tierShadow = nil
		}
		return
	}
	if e.tierShadow == nil {
		e.tierShadow = &tierShadowMetrics{host: e.host, model: e.modelName,
			client: &http.Client{Timeout: tierShadowPollTimeout}}
	}
	s := e.tierShadow
	s.mu.Lock()
	for policy := range policies {
		policies[policy] = s.queues[policy]
	}
	s.queues = policies
	if time.Since(s.sample.collected) <= sampleStalenessLimit {
		s.updateQueuesLocked()
	}
	s.mu.Unlock()
	if s.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel, s.done = cancel, make(chan struct{})
		go s.run(ctx)
	}
}

func (s *tierShadowMetrics) stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
	tierShadowBackendRequests.DeleteLabelValues(s.model, s.host, shadowRunning)
	tierShadowBackendRequests.DeleteLabelValues(s.model, s.host, shadowWaiting)
	tierShadowSampleTimestamp.DeleteLabelValues(s.model, s.host)
}

func (s *tierShadowMetrics) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(defaultMetricsPollInterval)
	defer ticker.Stop()
	for {
		s.scrape(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *tierShadowMetrics) scrape(ctx context.Context) {
	result := shadowScrapeError
	defer func() { tierShadowScrapes.WithLabelValues(s.model, result).Inc() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s.host+"/metrics", nil)
	if err != nil {
		return
	}
	if key, _ := pollAPIKey.Load().(string); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	parser := expfmt.NewTextParser(prommodel.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return
	}
	sample := tierShadowSample{collected: time.Now()}
	for _, metric := range []struct {
		state   string
		value   *float64
		present *bool
	}{
		{shadowWaiting, &sample.waiting, &sample.hasWaiting},
		{shadowRunning, &sample.running, &sample.hasRunning},
	} {
		for _, value := range families[shadowRequestMetricPrefix+metric.state].GetMetric() {
			var count float64
			switch {
			case value.Gauge != nil:
				count = value.Gauge.GetValue()
			case value.Untyped != nil:
				count = value.Untyped.GetValue()
			default:
				return
			}
			if math.IsNaN(count) || math.IsInf(count, 0) || count < 0 {
				return
			}
			*metric.value += count
			*metric.present = true
		}
		if *metric.present {
			tierShadowBackendRequests.WithLabelValues(s.model, s.host, metric.state).Set(*metric.value)
		} else {
			tierShadowBackendRequests.DeleteLabelValues(s.model, s.host, metric.state)
		}
	}
	s.mu.Lock()
	s.sample = sample
	s.updateQueuesLocked()
	s.mu.Unlock()
	tierShadowSampleTimestamp.WithLabelValues(s.model, s.host).Set(float64(sample.collected.Unix()))
	result = shadowScrapeComplete
	if !sample.hasWaiting || !sample.hasRunning {
		result = shadowScrapePartial
	}
}

func (s *tierShadowMetrics) updateQueuesLocked() {
	if !s.sample.hasWaiting {
		return
	}
	for policy, overloaded := range s.queues {
		trip, clear := policy.Marks()
		s.queues[policy] = trip > 0 && (s.sample.waiting >= float64(trip) || overloaded && s.sample.waiting > float64(clear))
	}
}
