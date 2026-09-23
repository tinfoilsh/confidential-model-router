package manager

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/billing"
)

func TestPrivacyFilterProxyDoesNotBillTwice(t *testing.T) {
	for _, receipt := range []string{"", "1"} {
		t.Run("receipt="+receipt, func(t *testing.T) {
			var reported atomic.Int64
			ingestion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reported.Add(1) }))
			defer ingestion.Close()
			collector := billing.NewCollector(ingestion.URL, "test-router", "test-secret")
			defer collector.Stop()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if receipt != "" {
					w.Header().Set(billableRequestsHeader, receipt)
				}
				_, _ = io.WriteString(w, `{"detected_spans":[],"redacted_text":"hello"}`)
			}))
			defer backend.Close()
			target, _ := url.Parse(backend.URL)
			proxy := newProxy(target.Host, "", privacyFilterModel, collector, newCircuitBreaker())
			proxy.Transport = http.DefaultTransport
			proxy.Director = func(req *http.Request) { req.URL.Scheme, req.URL.Host = target.Scheme, target.Host }
			req := httptest.NewRequest(http.MethodPost, "/redact", strings.NewReader(`{"text":"hello"}`))
			req.Header.Set("Authorization", "Bearer tk_customer")
			req.Header.Set(UsageMetricsRequestHeader, "true")
			recorder := httptest.NewRecorder()
			writer := &usageMetricsWriter{ResponseWriter: recorder, pricing: &ModelPricing{RequestPrice: 0.005}}
			req = req.WithContext(context.WithValue(req.Context(), usageWriterKey{}, writer))
			proxy.ServeHTTP(writer, req)
			collector.Stop()
			if reported.Load() != 0 {
				t.Fatal("router emitted a duplicate charge")
			}
			metrics := recorder.Header().Get(UsageMetricsResponseHeader)
			if strings.Contains(metrics, "cost_usd=") != (receipt == "1") {
				t.Fatalf("unexpected metrics: %s", metrics)
			}
			if receipt == "1" && !strings.Contains(metrics, "cost_usd=0.005") {
				t.Fatal("endpoint fee not reflected")
			}
		})
	}
}
