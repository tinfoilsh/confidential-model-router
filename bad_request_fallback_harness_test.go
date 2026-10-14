//go:build localharness

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

func TestRouterBadRequestFallback(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var admissions, rejected, accepted atomic.Int64
			var mu sync.Mutex
			var originalBody []byte
			em, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				admissions.Add(1)
				io.WriteString(w, `{"org_id":"org_admission_test","vllm_priority":3,"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rejected.Add(1)
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				originalBody = body
				mu.Unlock()
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"Per-request options disabled","type":"invalid_request_error"}}`)
			}), "", false)
			model, _ := em.GetModel(admissionTestModel)
			originalHost := ""
			for host := range model.Enclaves {
				originalHost = host
			}
			want := `{"output":"OK"}`
			if streaming {
				want = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
			}
			alternate := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				accepted.Add(1)
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				if string(body) != string(originalBody) {
					t.Errorf("retry did not preserve the final routed body: %s versus %s", body, originalBody)
				}
				mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer tk_test" {
					t.Error("retry lost authorization")
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				io.WriteString(w, want)
			}))
			t.Cleanup(alternate.Close)
			// Both local certificates are trusted; the production transport also
			// pins each alternate's independently registered public key.
			transport := http.DefaultTransport.(*http.Transport)
			transport.TLSClientConfig.RootCAs.AddCert(alternate.Certificate())
			if err := manager.AddFakeEnclaveForTest(em, admissionTestModel, alternate); err != nil {
				t.Fatal(err)
			}
			alternateHost := strings.TrimPrefix(alternate.URL, "https://")
			if err := manager.ConfigureReservationsForTest(em, admissionTestModel, []config.ReservationConfig{{OrgIDs: []string{admissionTestOrg}, Enclaves: []string{originalHost}}}, []string{originalHost, alternateHost}); err != nil {
				t.Fatal(err)
			}
			handler.warm("tk_test", admissionTestModel)
			admissionsBefore := admissions.Load()
			body := fmt.Sprintf(`{"model":"gpt-oss-120b","input":"Reply OK.","stream":%t,"mm_processor_kwargs":{"max_soft_tokens":280}}`, streaming)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, admissionRequest("/v1/responses", body, "tk_test"))
			if recorder.Code != http.StatusOK || recorder.Body.String() != want || recorder.Header().Get("Tinfoil-Enclave") != alternateHost {
				t.Fatalf("fallback: %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			if rejected.Load() != 1 || accepted.Load() != 1 || admissions.Load()-admissionsBefore != 1 {
				t.Fatalf("backend/admission counts: %d/%d/%d", rejected.Load(), accepted.Load(), admissions.Load()-admissionsBefore)
			}
			if got := testutil.ToFloat64(manager.CircuitBreakerState.WithLabelValues(admissionTestModel, originalHost)); got != 1 {
				t.Fatalf("rejected backend breaker = %v", got)
			}
			if got := testutil.ToFloat64(manager.ProxySuccessTotal.WithLabelValues(admissionTestModel, alternateHost)); got != 1 {
				t.Fatalf("accepted request counted %v times", got)
			}
		})
	}
}

func TestRouterNonStreamingUpstreamErrorOutcome(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"synthetic rejection","type":"invalid_request_error"}}`)
			}), "", false)
			outcome := "http_4xx"
			if status >= http.StatusInternalServerError {
				outcome = "error"
			}
			metric := manager.RequestDurationSeconds.WithLabelValues(admissionTestModel, "none", "none", "non_streaming", outcome)
			before, _ := histogramState(t, metric)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, admissionRequest("/v1/responses", `{"model":"gpt-oss-120b","input":"hi"}`, "tk_test"))
			after, _ := histogramState(t, metric)
			if recorder.Code != status || after != before+1 {
				t.Fatalf("HTTP %d outcome count %d -> %d", recorder.Code, before, after)
			}
		})
	}
}
