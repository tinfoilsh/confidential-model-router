package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
	usageclient "github.com/tinfoilsh/usage-reporting-go/client"
)

// Event represents a billing event with token usage
type Event struct {
	Timestamp        time.Time `json:"timestamp"`
	UserID           string    `json:"user_id"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	RequestID        string    `json:"request_id"`
	Enclave          string    `json:"enclave"`
	RequestPath      string    `json:"request_path"`
	Streaming        bool      `json:"streaming"`
	APIKey           string    `json:"api_key"`
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

		c.reporter.AddEvent(usagereporting.Event{
			RequestID:  event.RequestID,
			OccurredAt: event.Timestamp,
			APIKey:     event.APIKey,
			Operation: usagereporting.Operation{
				Service: usagereporting.ServiceRouter,
				Name:    usagereporting.OperationRouterModelRequest,
			},
			CustomerRequests: 1,
			Meters: []usagereporting.Meter{
				{Name: usagereporting.MeterInputTokens, Quantity: inputTokens},
				{Name: usagereporting.MeterOutputTokens, Quantity: outputTokens},
			},
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
