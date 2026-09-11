package billing

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

func TestUnknownModelReporterSignsAndDedupes(t *testing.T) {
	const secret = "test-secret"
	reports := make(chan UnknownModelReport, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if r.URL.Path != UnknownModelPath {
			t.Errorf("path = %q, want %q", r.URL.Path, UnknownModelPath)
		}
		reporterID, ts, nonce, sig, err := usagereporting.HeaderValues(r.Header)
		if err != nil {
			t.Errorf("headers: %v", err)
		}
		if !usagereporting.VerifyBatch(r.Method, r.URL.Path, reporterID, ts, nonce, body, secret, sig) {
			t.Error("signature did not verify")
		}
		var report UnknownModelReport
		if err := json.Unmarshal(body, &report); err != nil {
			t.Errorf("decode: %v", err)
		}
		reports <- report
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	r := NewUnknownModelReporter(server.URL, "router-test", secret)
	for range 3 {
		r.Report("tk_abc", "kimi-k2-5")
	}
	r.Report("tk_abc", "kimi-k2-6")
	r.Report("", "kimi-k2-5")
	r.Report("tk_abc", "")

	got := map[string]bool{}
	for range 2 {
		select {
		case rep := <-reports:
			if rep.APIKey != "tk_abc" {
				t.Errorf("api_key = %q", rep.APIKey)
			}
			got[rep.Model] = true
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for report")
		}
	}
	if !got["kimi-k2-5"] || !got["kimi-k2-6"] {
		t.Errorf("models reported = %v", got)
	}
	select {
	case rep := <-reports:
		t.Errorf("unexpected extra report %+v", rep)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestUnknownModelReporterNilWhenUnconfigured(t *testing.T) {
	var r *UnknownModelReporter = NewUnknownModelReporter("https://cp", "id", "")
	if r != nil {
		t.Fatal("expected nil reporter without a secret")
	}
	r.Report("tk_abc", "kimi-k2-5")
}
