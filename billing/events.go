package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	usageclient "github.com/tinfoilsh/usage-reporting-go/client"
	"github.com/tinfoilsh/usage-reporting-go/contract"
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

// Collector collects billing events in memory and sends them to the control plane
type Collector struct {
	events   []Event
	mu       sync.Mutex
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
func NewCollector(controlPlaneURL, reporterID, reporterSecret string) *Collector {
	endpoint := ""
	if controlPlaneURL != "" {
		endpoint = strings.TrimRight(controlPlaneURL, "/") + "/api/internal/usage-reports"
	}

	c := &Collector{
		events: make([]Event, 0),
		reporter: usageclient.New(usageclient.Config{
			Endpoint: endpoint,
			Reporter: contract.Reporter{
				ID:      reporterID,
				Service: "router",
			},
			Secret: reporterSecret,
		}),
	}
	return c
}

// AddEvent adds a billing event and logs it
func (c *Collector) AddEvent(event Event) {
	// Create a safe version for logging with masked API key
	safeEvent := event
	safeEvent.APIKey = maskAPIKey(event.APIKey)

	// Perform JSON marshalling outside the critical section
	eventJSON, err := json.Marshal(safeEvent)
	if err != nil {
		log.WithError(err).Error("Failed to marshal billing event")
		return
	}

	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()

	if c.reporter != nil {
		c.reporter.AddEvent(contract.Event{
			RequestID:  event.RequestID,
			OccurredAt: event.Timestamp,
			APIKey:     event.APIKey,
			Operation: contract.Operation{
				Service: "router",
				Name:    "model_request",
			},
			Meters: []contract.Meter{
				{Name: "input_tokens", Quantity: int64(event.PromptTokens)},
				{Name: "output_tokens", Quantity: int64(event.CompletionTokens)},
				{Name: "requests", Quantity: 1},
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

// GetEvents returns all collected events (for testing/debugging)
func (c *Collector) GetEvents() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return a copy to avoid race conditions
	eventsCopy := make([]Event, len(c.events))
	copy(eventsCopy, c.events)
	return eventsCopy
}

// Clear removes all events (for testing)
func (c *Collector) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = c.events[:0]
}

// Stop gracefully shuts down the collector
func (c *Collector) Stop() {
	c.stopOnce.Do(func() {
		if c.reporter != nil {
			_ = c.reporter.Stop(context.Background())
		}
	})
}
