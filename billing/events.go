package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
	usageclient "github.com/tinfoilsh/usage-reporting-go/client"
)

// Emission counters pair with the proxy's router_request_* observation
// metrics: observed-vs-emitted localizes a loss to the usage handler, while
// the usage_reporter_* counters below localize it to enqueueing or delivery.
var (
	eventsEmitted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "router_billing_events_emitted_total",
		Help: "Billing events handed to the usage reporter, by model.",
	}, []string{"model"})
	promptTokensEmitted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "router_billing_prompt_tokens_emitted_total",
		Help: "Prompt tokens on billing events handed to the usage reporter, by model.",
	}, []string{"model"})

	reporterStatsOnce sync.Once
)

// registerReporterStats exports the reporter's delivery accounting as
// Prometheus counters. Registered once for the process-lifetime collector;
// extra collectors (tests) are ignored.
func registerReporterStats(reporter *usageclient.ReporterClient) {
	reporterStatsOnce.Do(func() {
		counter := func(name, help string, read func(usageclient.Stats) uint64) prometheus.CounterFunc {
			return prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, func() float64 {
				return float64(read(reporter.Stats()))
			})
		}
		prometheus.MustRegister(
			counter("usage_reporter_enqueued_events_total", "Events accepted into the reporter buffer.", func(s usageclient.Stats) uint64 { return s.Enqueued }),
			counter("usage_reporter_delivered_events_total", "Events in batches acknowledged with HTTP 2xx.", func(s usageclient.Stats) uint64 { return s.DeliveredEvents }),
			counter("usage_reporter_delivered_batches_total", "Batches acknowledged with HTTP 2xx.", func(s usageclient.Stats) uint64 { return s.DeliveredBatches }),
			counter("usage_reporter_failed_events_total", "Events discarded with a failed batch (no retry).", func(s usageclient.Stats) uint64 { return s.FailedEvents }),
			counter("usage_reporter_failed_batches_total", "Batches that failed delivery and were discarded.", func(s usageclient.Stats) uint64 { return s.FailedBatches }),
			counter("usage_reporter_dropped_buffer_full_total", "Oldest events overwritten on buffer overflow.", func(s usageclient.Stats) uint64 { return s.DroppedBufferFull }),
			counter("usage_reporter_dropped_disabled_total", "Events discarded because the reporter is not configured.", func(s usageclient.Stats) uint64 { return s.DroppedDisabled }),
		)
	})
}

// Event represents a billing event with token usage. CachedPromptTokens is the
// subset of PromptTokens that the model served from its prompt cache; the
// uncached portion is derived downstream.
type Event struct {
	Timestamp          time.Time `json:"timestamp"`
	UserID             string    `json:"user_id"`
	Model              string    `json:"model"`
	PromptTokens       int       `json:"prompt_tokens"`
	CachedPromptTokens int       `json:"cached_prompt_tokens"`
	CompletionTokens   int       `json:"completion_tokens"`
	TotalTokens        int       `json:"total_tokens"`
	RequestID          string    `json:"request_id"`
	Enclave            string    `json:"enclave"`
	RequestPath        string    `json:"request_path"`
	Streaming          bool      `json:"streaming"`
	APIKey             string    `json:"api_key"`
}

// Collector ships billing events to the control plane via the usage reporter.
type Collector struct {
	reporter *usageclient.ReporterClient
	stopOnce sync.Once
}

// maskAPIKey masks an API key for safe logging
// Shows first 3 and last 4 characters, masking the rest
func maskAPIKey(apiKey string) string {
	if len(apiKey) <= 10 {
		// Too short to mask safely
		return "***"
	}
	return apiKey[:3] + strings.Repeat("*", len(apiKey)-7) + apiKey[len(apiKey)-4:]
}

// NewCollector creates a new billing event collector.
//
// Events are delivered to the signed usage-reports ingestion endpoint
// using the shared usage-reporting client.
func NewCollector(controlPlaneURL, reporterID, reporterSecret string) *Collector {
	endpoint := ""
	if controlPlaneURL != "" {
		endpoint = strings.TrimRight(controlPlaneURL, "/") + usagereporting.IngestionPath
	}

	c := &Collector{
		reporter: usageclient.New(usageclient.Config{
			Endpoint:   endpoint,
			ReporterID: reporterID,
			Secret:     reporterSecret,
		}),
	}
	registerReporterStats(c.reporter)
	return c
}

// AddEvent forwards a billing event to the usage reporter and writes a
// masked log line for local observability.
func (c *Collector) AddEvent(event Event) {
	// Create a safe version for logging with masked API key
	safeEvent := event
	safeEvent.APIKey = maskAPIKey(event.APIKey)

	eventJSON, err := json.Marshal(safeEvent)
	if err != nil {
		log.WithError(err).Error("Failed to marshal billing event")
		return
	}

	if c.reporter != nil {
		inputTokens := int64(event.PromptTokens)
		outputTokens := int64(event.CompletionTokens)
		if inputTokens == 0 && outputTokens == 0 && event.TotalTokens > 0 {
			inputTokens = int64(event.TotalTokens)
		}

		cachedInputTokens := int64(event.CachedPromptTokens)
		if cachedInputTokens > inputTokens {
			cachedInputTokens = inputTokens
		}

		meters := []usagereporting.Meter{
			{Name: usagereporting.MeterInputTokens, Quantity: inputTokens},
			{Name: usagereporting.MeterOutputTokens, Quantity: outputTokens},
		}
		if cachedInputTokens > 0 {
			meters = append(meters, usagereporting.Meter{
				Name:     usagereporting.MeterCachedInputTokens,
				Quantity: cachedInputTokens,
			})
		}

		eventsEmitted.WithLabelValues(event.Model).Inc()
		promptTokensEmitted.WithLabelValues(event.Model).Add(float64(inputTokens))

		c.reporter.AddEvent(usagereporting.Event{
			RequestID:  event.RequestID,
			OccurredAt: event.Timestamp,
			APIKey:     event.APIKey,
			Operation: usagereporting.Operation{
				Service: usagereporting.ServiceRouter,
				Name:    usagereporting.OperationRouterModelRequest,
			},
			CustomerRequests: 1,
			Meters:           meters,
			Attributes: map[string]string{
				"model":     event.Model,
				"route":     event.RequestPath,
				"streaming": fmt.Sprintf("%t", event.Streaming),
				"enclave":   event.Enclave,
			},
		})
	}

	log.WithFields(log.Fields{
		"type": "billing_event",
		"data": string(eventJSON),
	}).Info("Billing event collected")
}

// Stop gracefully shuts down the collector
func (c *Collector) Stop() {
	c.stopOnce.Do(func() {
		if c.reporter != nil {
			c.reporter.Stop(context.Background())
		}
	})
}
