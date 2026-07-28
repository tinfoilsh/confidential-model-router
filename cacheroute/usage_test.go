package cacheroute

import (
	"context"
	"testing"
)

func TestObserveReturnsReuseClassification(t *testing.T) {
	s, _, _ := newTestShadow(t)
	req := keyedRequest(t, "usage-class", 100)
	cfg := defaultSettings()

	if got := s.Observe("m", req, pool2(), "enclave-a", cfg); got != ReuseFirstSeen {
		t.Fatalf("first observation: got %q, want %q", got, ReuseFirstSeen)
	}
	if got := s.Observe("m", req, pool2(), "enclave-a", cfg); got != ReuseRepeatWarm {
		t.Fatalf("repeat on warm host: got %q, want %q", got, ReuseRepeatWarm)
	}
	if got := s.Observe("m", req, pool2(), "enclave-b", cfg); got != ReuseRepeatCold {
		t.Fatalf("repeat on cold host: got %q, want %q", got, ReuseRepeatCold)
	}
}

func TestObserveReturnsEmptyWhenNotClassified(t *testing.T) {
	s, _, _ := newTestShadow(t)
	req := keyedRequest(t, "usage-small-pool", 100)
	cfg := defaultSettings()

	small := Pool{Size: 1, Candidates: hosts("enclave-a")}
	if got := s.Observe("m", req, small, "enclave-a", cfg); got != "" {
		t.Fatalf("pool too small: got %q, want empty", got)
	}

	req.Outcome = OutcomeNoSalt
	if got := s.Observe("m", req, pool2(), "enclave-a", cfg); got != "" {
		t.Fatalf("non-keyed request: got %q, want empty", got)
	}
}

func TestRecordUsage(t *testing.T) {
	ctx := WithReuse(context.Background(), "usage-test-model", ReuseRepeatCold)
	RecordUsage(ctx, 1000, 250)
	RecordUsage(ctx, 500, 0)

	if got := counterValue(t, UsagePromptTokensTotal, "usage-test-model", ReuseRepeatCold); got != 1500 {
		t.Fatalf("prompt tokens: got %v, want 1500", got)
	}
	if got := counterValue(t, UsageCachedPromptTokensTotal, "usage-test-model", ReuseRepeatCold); got != 250 {
		t.Fatalf("cached tokens: got %v, want 250", got)
	}
}

func TestRecordUsageWithoutClassificationIsNoop(t *testing.T) {
	RecordUsage(context.Background(), 1000, 250)

	if got := counterValue(t, UsagePromptTokensTotal, "usage-noop-model", ReuseFirstSeen); got != 0 {
		t.Fatalf("prompt tokens without classification: got %v, want 0", got)
	}
}
