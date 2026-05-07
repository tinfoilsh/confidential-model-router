package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinfoilsh/usage-reporting-go/contract"
)

func TestCollectorAddsCustomerRequestCounter(t *testing.T) {
	batches := make(chan contract.Batch, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var batch contract.Batch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode usage batch: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		batches <- batch
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	collector := NewCollector(server.URL, "reporter", "secret")
	defer collector.Stop()
	collector.AddEvent(Event{
		Timestamp:        time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		APIKey:           "tk_test",
		Model:            "gpt-oss-120b",
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		RequestID:        "request-1",
		RequestPath:      "/v1/chat/completions",
	})
	if err := collector.reporter.Flush(context.Background()); err != nil {
		t.Fatalf("flush usage reporter: %v", err)
	}

	select {
	case batch := <-batches:
		if len(batch.Events) != 1 {
			t.Fatalf("expected one event, got %d", len(batch.Events))
		}
		event := batch.Events[0]
		if got := meterQuantity(event, "requests"); got != 1 {
			t.Fatalf("request meter mismatch: got %d want 1", got)
		}
		if got := counterQuantity(event, contract.CounterCustomerRequests); got != 1 {
			t.Fatalf("customer request counter mismatch: got %d want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for usage batch")
	}
}

func meterQuantity(event contract.Event, name string) int64 {
	for _, meter := range event.Meters {
		if meter.Name == name {
			return meter.Quantity
		}
	}
	return 0
}

func counterQuantity(event contract.Event, name string) int64 {
	for _, counter := range event.Counters {
		if counter.Name == name {
			return counter.Quantity
		}
	}
	return 0
}
