//go:build localharness

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/manager"
	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

func TestRouterRetries502AcrossReplicas(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			var events []usagereporting.Event
			capture := func(r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				bodies = append(bodies, string(body))
				mu.Unlock()
			}
			em, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == usagereporting.IngestionPath {
					var batch usagereporting.Batch
					if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
						t.Error(err)
					}
					mu.Lock()
					events = append(events, batch.Events...)
					mu.Unlock()
					return
				}
				fmt.Fprintf(w, `{"org_id":%q,"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`, admissionTestOrg)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capture(r)
				http.Error(w, "engine unavailable", http.StatusBadGateway)
			}), admissionTestOrg, false)
			model, _ := em.GetModel(admissionTestModel)
			var firstHost string
			for host := range model.Enclaves {
				firstHost = host
			}
			secondHost := addAdmissionBackend(t, em, admissionTestModel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := testutil.ToFloat64(manager.BackendInflight.WithLabelValues(admissionTestModel, firstHost)); got != 0 {
					t.Errorf("failed replica retained inflight=%v during retry", got)
				}
				capture(r)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n")
					w.(http.Flusher).Flush()
					io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"choices":[{"message":{"content":"recovered"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
				}
			}))
			if err := manager.ConfigureReservationsForTest(em, admissionTestModel, model.Reservations, []string{firstHost, secondHost}); err != nil {
				t.Fatal(err)
			}
			stopBilling := manager.EnableBillingForTest(em)
			t.Cleanup(stopBilling)
			request := admissionRequest("/v1/chat/completions", fmt.Sprintf(`{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hello"}],"stream":%t}`, streaming), usageContextTestKey)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			handler.client.refreshes.Wait()
			stopBilling()
			mu.Lock()
			defer mu.Unlock()
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "recovered") || recorder.Header().Get("Tinfoil-Enclave") != secondHost {
				t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			if len(bodies) != 2 || bodies[0] != bodies[1] {
				t.Fatalf("requests=%q", bodies)
			}
			if len(events) != 1 || events[0].CustomerRequests != 1 || events[0].Attributes["enclave"] != secondHost {
				t.Fatalf("billing events=%+v", events)
			}
			for _, host := range []string{firstHost, secondHost} {
				if got := testutil.ToFloat64(manager.BackendInflight.WithLabelValues(admissionTestModel, host)); got != 0 {
					t.Errorf("%s retained inflight=%v", host, got)
				}
			}
			if got := testutil.ToFloat64(manager.ProxyFailureTotal.WithLabelValues(admissionTestModel, firstHost, "http_502")); got != 1 {
				t.Errorf("first failure count=%v", got)
			}
			if streaming {
				if count, _ := histogramState(t, manager.FirstTokenSeconds.WithLabelValues(admissionTestModel, secondHost, "spilled", "none")); count != 1 {
					t.Errorf("retry first-token observations=%d", count)
				}
				if count, _ := histogramState(t, manager.FirstTokenSeconds.WithLabelValues(admissionTestModel, firstHost, "reserved", "none")); count != 0 {
					t.Errorf("first-token observation attributed to failed replica: %d", count)
				}
			}
		})
	}
}
