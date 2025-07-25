package billing

import (
	"encoding/json"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
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
}

// Collector collects billing events in memory and logs them
type Collector struct {
	events []Event
	mu     sync.Mutex
}

// NewCollector creates a new billing event collector
func NewCollector() *Collector {
	return &Collector{
		events: make([]Event, 0),
	}
}

// AddEvent adds a billing event and logs it
func (c *Collector) AddEvent(event Event) {
	// Perform JSON marshalling outside the critical section
	eventJSON, err := json.Marshal(event)
	if err != nil {
		log.WithError(err).Error("Failed to marshal billing event")
		return
	}

	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()

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
