//go:build localharness

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The plain proxy and the tool loop each re-encode the request. Both must
// forward the client's schema and tool bytes, whose key order is the order
// the model generates fields in.
func TestRouterForwardsSchemasInClientOrder(t *testing.T) {
	const clientSchema = `{"type":"object","properties":{"reasoning":{"type":"string"},"answer":{"type":"string","enum":["b","a"],"description":"Pick <b> & explain."}},"required":["reasoning","answer"]}`
	const clientTool = `{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}}}`
	const autoContinueTool = `{"type":"function","function":{"name":"show","parameters":{"type":"object"},"x-tinfoil-tool-auto-continue":true}}`
	for _, tc := range []struct{ name, tools string }{
		{"plain proxy", clientTool},
		{"tool loop", clientTool + "," + autoContinueTool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forwarded := make(chan []byte, 1)
			_, handler := newAdmissionHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`)
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				forwarded <- data
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(billingLoopResponse("/v1/chat/completions", 1, true, ""))
			}), "", false)

			body := `{"model":"gpt-oss-120b","messages":[{"role":"user","content":"hi"}],` +
				`"response_format":{"type":"json_schema","json_schema":{"name":"verdict","schema":` + clientSchema + `}},` +
				`"tools":[` + tc.tools + `]}`
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, admissionRequest("/v1/chat/completions", body, "tk_test"))
			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
			}

			data := <-forwarded
			for _, client := range []string{`"schema":` + clientSchema, clientTool} {
				// encoding/json escapes <, > and & even in raw values.
				var escaped bytes.Buffer
				json.HTMLEscape(&escaped, []byte(client))
				if !bytes.Contains(data, escaped.Bytes()) {
					t.Errorf("forwarded body lost the client's encoding of %s:\n%s", client, data)
				}
			}
		})
	}
}
