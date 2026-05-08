package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinfoilsh/usage-reporting-go/contract"
)

// TestCollectorEmitsCustomerRequestAndTokenMeters verifies that a billing
// event flushed through the reporter carries CustomerRequests=1 and exactly
// the canonical input/output token meters (no requests meter).
func TestCollectorEmitsCustomerRequestAndTokenMeters(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		bodies <- buf
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewCollector(server.URL, "router-test", "test-secret")
	defer c.Stop()

	c.AddEvent(Event{
		Timestamp:        time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		Model:            "gpt-oss-120b",
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		RequestID:        "req-1",
		Enclave:          "enclave-1",
		RequestPath:      "/v1/chat/completions",
		Streaming:        true,
		APIKey:           "sk-test-1234567890",
	})

	c.reporter.Flush(context.Background())

	var body []byte
	select {
	case body = <-bodies:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for billing event delivery")
	}

	var batch contract.Batch
	if err := json.Unmarshal(body, &batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(batch.Events))
	}
	got := batch.Events[0]

	if got.Operation.Service != contract.ServiceRouter {
		t.Errorf("service mismatch: got %q want %q", got.Operation.Service, contract.ServiceRouter)
	}
	if got.Operation.Name != contract.OperationRouterModelRequest {
		t.Errorf("operation mismatch: got %q want %q", got.Operation.Name, contract.OperationRouterModelRequest)
	}
	if got.CustomerRequests != 1 {
		t.Errorf("customer requests mismatch: got %d want 1", got.CustomerRequests)
	}

	meters := make(map[string]int64)
	for _, m := range got.Meters {
		meters[m.Name] = m.Quantity
	}
	if meters[contract.MeterInputTokens] != 11 {
		t.Errorf("input_tokens mismatch: got %d want 11", meters[contract.MeterInputTokens])
	}
	if meters[contract.MeterOutputTokens] != 7 {
		t.Errorf("output_tokens mismatch: got %d want 7", meters[contract.MeterOutputTokens])
	}
	if _, hasRequestsMeter := meters["requests"]; hasRequestsMeter {
		t.Errorf("billing event should not emit a requests meter; got %+v", meters)
	}

	if got.Attributes["model"] != "gpt-oss-120b" {
		t.Errorf("model attribute mismatch: got %q", got.Attributes["model"])
	}
	if got.Attributes["streaming"] != "true" {
		t.Errorf("streaming attribute mismatch: got %q", got.Attributes["streaming"])
	}
}
