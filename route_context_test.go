package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

const admissionTestModel = "gpt-oss-120b"

func accessTokenForTest(header, payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".c2ln"
}

func TestRouteContextUncachedDecisions(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != routeContextPath || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected admission request: %s %s", r.Method, r.URL.Path)
		}
		var req routeContextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.APIKey != "tk_test" || req.Model != admissionTestModel {
			t.Errorf("bad request: %+v, %v", req, err)
		}
		n := calls.Add(1)
		decision := decisionAllowed
		if n == 2 {
			decision = decisionDemote
		}
		if n == 3 {
			decision = decisionRejected
		}
		reason := ""
		if decision != decisionAllowed {
			reason = rateReasonRequests
		}
		fmt.Fprintf(w, `{"org_id":"org_test","rate_limit":{"decision":%q,"reason":%q,"retry_after_seconds":30,"requests":{"limit":2,"used":%d}}}`, decision, reason, n)
	}))
	defer server.Close()
	client := newRouteContextClient(server.URL)
	for i, want := range []string{decisionAllowed, decisionDemote, decisionRejected} {
		got, err := client.Lookup(context.Background(), "tk_test", admissionTestModel)
		if want == decisionRejected {
			if err == nil || err.apiError.Status != http.StatusTooManyRequests || err.retryAfter != "30" {
				t.Fatalf("rejection: %v", err)
			}
		} else if err != nil || got.RateLimit.Decision != want || got.OrgID != "org_test" || *got.RateLimit.Requests.Used != int64(i+1) {
			t.Fatalf("decision %s: %+v, %v", want, got, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("counting calls = %d", calls.Load())
	}
}

func TestRouteContextInvalidResponsesAdmitWithoutDecision(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `not json`, `{"priority":-1}`, `{"rate_limit":null}`,
		`{"rate_limit":{"decision":"allowed"}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":null}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":-1}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":1.5}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":"30"}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":9223372036854775808}}`,
		`{"rate_limit":{"decision":"unknown","retry_after_seconds":0}}`,
		`{"rate_limit":{"decision":"ALLOWED","retry_after_seconds":0}}`,
		`{"rate_limit":{"decision":null,"retry_after_seconds":0}}`,
		`{"rate_limit":{"decision":"rejected","reason":"other","retry_after_seconds":1}}`,
		`{"rate_limit":{"decision":"rejected","reason":"tokens","retry_after_seconds":-1}}`,
		`{"rate_limit":{"decision":"rejected","reason":"requests","retry_after_seconds":0}}`,
		`{"rate_limit":{"decision":"rejected","retry_after_seconds":1}}`,
		`{"rate_limit":{"decision":"rejected","reason":null,"retry_after_seconds":1}}`,
		`{"rate_limit":{"decision":"rejected","reason":42,"retry_after_seconds":1}}`,
		`{"rate_limit":{"decision":"demote","retry_after_seconds":1}}`,
		`{"rate_limit":{"decision":"demote","reason":"tokens","retry_after_seconds":1}}`,
		`{"rate_limit":{"decision":"allowed","reason":"requests","retry_after_seconds":0}}`,
		`{"rate_limit":{"decision":"exempt","reason":"tokens","retry_after_seconds":0}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":0,"requests":{"limit":10}}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":0,"tokens":{"limit":10,"used":-1}}}`,
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":0}} {}`,
		strings.Repeat(" ", routeContextResponseLimit+1),
	} {
		t.Run(fmt.Sprintf("case_%d", len(body))+body[:min(len(body), 70)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer server.Close()
			before := lookupFailures(t, admissionTestModel)
			resolved, err := newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
			assertAdmittedWithoutDecision(t, resolved, err)
			if lookupFailures(t, admissionTestModel) != before+1 {
				t.Fatal("invalid response not counted as a lookup failure")
			}
		})
	}
}

// lookupFailures sums RouteContextLookupFailuresTotal across reasons for one model.
func lookupFailures(t *testing.T, model string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(manager.RouteContextLookupFailuresTotal)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "model" && label.GetValue() == model {
					total += metric.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// assertAdmittedWithoutDecision checks the fail-open shape: no error, and no
// priority, org, or decision that could grant more than the shared default.
func assertAdmittedWithoutDecision(t *testing.T, resolved routeContext, err *routeContextError) {
	t.Helper()
	if err != nil {
		t.Fatalf("lookup outage rejected the request: %v", err)
	}
	if resolved.Priority != nil || resolved.OrgID != "" || resolved.RateLimit != nil || resolved.overloadExempt() {
		t.Fatalf("lookup outage granted context: %+v", resolved)
	}
}

func TestRouteContextStatusesAndRejections(t *testing.T) {
	for _, status := range []int{401, 402, 403, 404, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "-10")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":"credentials denied"}`)
			}))
			defer server.Close()
			failures := manager.RouteContextLookupFailuresTotal.WithLabelValues(admissionTestModel, fmt.Sprintf("http_%d", status))
			before := testutil.ToFloat64(failures)
			resolved, err := newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
			if status == 401 || status == 402 || status == 403 || status == 429 {
				if err == nil || err.apiError.Status != status {
					t.Fatalf("denial not forwarded: %v", err)
				}
				if testutil.ToFloat64(failures) != before {
					t.Fatal("denial counted as a lookup outage")
				}
				return
			}
			assertAdmittedWithoutDecision(t, resolved, err)
			if testutil.ToFloat64(failures) != before+1 {
				t.Fatal("status failure not counted as a lookup outage")
			}
		})
	}
	for _, reason := range []string{rateReasonRequests, rateReasonTokens} {
		beforeTotal := testutil.ToFloat64(manager.RateLimitRejectionsTotal.WithLabelValues(admissionTestModel))
		beforeReason := testutil.ToFloat64(manager.RateLimitRejectionsByReasonTotal.WithLabelValues(admissionTestModel, reason))
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"rate_limit":{"decision":"rejected","reason":%q,"retry_after_seconds":1}}`, reason)
		}))
		_, err := newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
		server.Close()
		if err == nil {
			t.Fatal("rejection admitted")
		}
		rec := httptest.NewRecorder()
		err.write(rec)
		message := "requests"
		if reason == rateReasonTokens {
			message = "tokens"
		}
		if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), "for "+message) {
			t.Fatalf("rejection response: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
		}
		if got := testutil.ToFloat64(manager.RateLimitRejectionsTotal.WithLabelValues(admissionTestModel)) - beforeTotal; got != 1 {
			t.Fatalf("compatibility rejection counter delta = %v", got)
		}
		if got := testutil.ToFloat64(manager.RateLimitRejectionsByReasonTotal.WithLabelValues(admissionTestModel, reason)) - beforeReason; got != 1 {
			t.Fatalf("%s rejection counter delta = %v", reason, got)
		}
	}
}

const quotaDenialTestBody = `{"error":{"message":"API key quota exhausted.","type":"insufficient_quota","code":"insufficient_quota","param":null}}`

var quotaRetryAfterCases = []struct {
	name   string
	values []string
	want   string
}{
	{"absent", nil, ""},
	{"seconds", []string{"30"}, "30"},
	{"zero seconds", []string{"0"}, "0"},
	{"HTTP date", []string{"Sun, 06 Nov 1994 08:49:37 GMT"}, "Sun, 06 Nov 1994 08:49:37 GMT"},
	{"negative", []string{"-1"}, ""},
	{"signed", []string{"+30"}, ""},
	{"fractional", []string{"1.5"}, ""},
	{"overflow", []string{"18446744073709551616"}, ""},
	{"invalid date", []string{"Sun, 99 Nov 1994 08:49:37 GMT"}, ""},
	{"garbage", []string{"soon"}, ""},
	{"multiple", []string{"30", "60"}, ""},
}

func TestRouteContextQuotaDenial(t *testing.T) {
	for _, tc := range quotaRetryAfterCases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			beforeFailures := testutil.ToFloat64(manager.RouteContextLookupFailuresTotal.WithLabelValues(admissionTestModel, "http_429"))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for _, value := range tc.values {
					w.Header().Add("Retry-After", value)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, quotaDenialTestBody)
			}))
			defer server.Close()
			client := newRouteContextClient(server.URL)
			for _, model := range []string{admissionTestModel, ""} {
				var err *routeContextError
				if model == "" {
					_, err = client.Metadata(context.Background(), "tk_test")
				} else {
					_, err = client.Lookup(context.Background(), "tk_test", model)
				}
				if err == nil {
					t.Fatal("quota denial admitted")
				}
				rec := httptest.NewRecorder()
				err.write(rec)
				assertQuotaDenial(t, rec, tc.want)
			}
			if calls.Load() != 2 {
				t.Fatalf("quota denial calls = %d, want 2", calls.Load())
			}
			if testutil.ToFloat64(manager.RouteContextLookupFailuresTotal.WithLabelValues(admissionTestModel, "http_429")) != beforeFailures {
				t.Fatal("quota denial counted as lookup outage")
			}
		})
	}
}

func assertQuotaDenial(t *testing.T, rec *httptest.ResponseRecorder, retryAfter string) {
	t.Helper()
	var envelope manager.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests || envelope.Error.Type != "insufficient_quota" || envelope.Error.Code == nil || *envelope.Error.Code != "insufficient_quota" || envelope.Error.Message != "API key quota exhausted." || envelope.Error.Param != nil {
		t.Fatalf("quota denial changed: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != retryAfter {
		t.Fatalf("Retry-After = %q, want %q", got, retryAfter)
	}
	if retryAfter == "" && len(rec.Header().Values("Retry-After")) != 0 {
		t.Fatal("unexpected retry header for lifetime cap")
	}
}

func TestRouteContextQuotaDenialHidesUnrecognizedBodies(t *testing.T) {
	for _, body := range []string{"<html>private upstream details</html>", `{"error":"private upstream details"}`, strings.Repeat("private upstream details", routeContextResponseLimit)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, body)
		}))
		_, err := newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
		server.Close()
		if err == nil || err.apiError.Status != http.StatusTooManyRequests || err.apiError.Message != http.StatusText(http.StatusTooManyRequests) || err.retryAfter != "" {
			t.Fatalf("unsafe quota error handling: %v", err)
		}
	}
}

func TestRouteContextDecisionReasonContract(t *testing.T) {
	for _, decision := range []string{decisionAllowed, decisionExempt, decisionDemote, decisionRejected, "unknown"} {
		for _, reason := range []string{"", rateReasonRequests, rateReasonTokens, "unknown"} {
			for _, retry := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/%s/%d", decision, reason, retry), func(t *testing.T) {
					valid := retry >= 0 && ((decision == decisionAllowed || decision == decisionExempt) && reason == "" || decision == decisionDemote && reason == rateReasonRequests || decision == decisionRejected && retry > 0 && (reason == rateReasonRequests || reason == rateReasonTokens))
					beforeSeries := testutil.CollectAndCount(manager.RateLimitRejectionsByReasonTotal)
					beforeRejections := testutil.ToFloat64(manager.RateLimitRejectionsTotal.WithLabelValues(admissionTestModel))
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						fmt.Fprintf(w, `{"rate_limit":{"decision":%q,"reason":%q,"retry_after_seconds":%d}}`, decision, reason, retry)
					}))
					defer server.Close()
					resolved, err := newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
					if !valid {
						assertAdmittedWithoutDecision(t, resolved, err)
						if testutil.CollectAndCount(manager.RateLimitRejectionsByReasonTotal) != beforeSeries || testutil.ToFloat64(manager.RateLimitRejectionsTotal.WithLabelValues(admissionTestModel)) != beforeRejections {
							t.Fatal("malformed decision emitted a quota rejection metric")
						}
					} else if decision == decisionRejected {
						if err == nil || err.apiError.Status != 429 {
							t.Fatalf("valid rejection failed: %v", err)
						}
					} else if err != nil {
						t.Fatalf("valid decision failed: %v", err)
					}
				})
			}
		}
	}
}

func TestRouteContextQuotaDetailsDoNotOverrideDecision(t *testing.T) {
	for _, payload := range []string{
		`{"rate_limit":{"decision":"allowed","retry_after_seconds":0,"requests":{"limit":0,"used":10},"tokens":{"limit":1,"used":100}}}`,
		`{"rate_limit":{"decision":"exempt","retry_after_seconds":0}}`,
		`{"rate_limit":{"decision":"demote","reason":"requests","retry_after_seconds":1,"requests":{"limit":100,"used":0}}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload) }))
		_, err := newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
		server.Close()
		if err != nil {
			t.Fatalf("valid CP decision overridden by optional details: %v", err)
		}
	}
}

func TestRouteContextMetadataAndJWTClassification(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["model"]; ok {
			t.Error("metadata lookup contained model")
		}
		io.WriteString(w, `{"org_id":"org_test","priority":-1}`)
	}))
	defer server.Close()
	client := newRouteContextClient(server.URL)
	got, err := client.Metadata(context.Background(), "tk_test")
	if err != nil || got.OrgID != "org_test" {
		t.Fatalf("metadata: %+v %v", got, err)
	}
	for _, typ := range []string{"at+jwt", "application/at+jwt", "AT+JWT"} {
		jwt := accessTokenForTest(fmt.Sprintf(`{"typ":%q}`, typ), `{"sub":"user_test"}`)
		if _, err := client.Lookup(context.Background(), jwt, admissionTestModel); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Metadata(context.Background(), jwt); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("JWT made %d route-context calls", calls.Load()-1)
	}
	for _, token := range []string{
		"opaque", "a.b.c", "", accessTokenForTest(`{"typ":"JWT"}`, `{"sub":"user"}`),
		accessTokenForTest(`{"typ":"at+jwt"}`, `{"sub":42}`),
		accessTokenForTest(`{"typ":"at+jwt"}`, `{broken`),
		accessTokenForTest(`{"typ":"at+jwt"}`, `{}`),
		accessTokenForTest(`{"typ":"at+jwt"}`, `{"sub":"user"}`) + "!",
	} {
		if jwtSubject(token) != "" {
			t.Errorf("malformed/untyped bearer bypassed admission: %q", token)
		}
	}
	var nilClient *routeContextClient
	if _, err := nilClient.Lookup(context.Background(), "", admissionTestModel); err == nil || err.apiError.Status != 401 {
		t.Fatalf("missing key: %v", err)
	}
	resolved, err := nilClient.Lookup(context.Background(), "tk_test", admissionTestModel)
	assertAdmittedWithoutDecision(t, resolved, err)
}

func TestRouteContextTimeoutAndNoRedirectReplay(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == routeContextPath {
			http.Redirect(w, r, "/replay", http.StatusTemporaryRedirect)
			return
		}
		t.Error("admission redirect followed")
	}))
	resolved, err := newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
	server.Close()
	assertAdmittedWithoutDecision(t, resolved, err)
	if calls.Load() != 1 {
		t.Fatalf("redirect replayed: %d calls", calls.Load())
	}
	resolved, err = newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
	assertAdmittedWithoutDecision(t, resolved, err)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	transportFailures := manager.RouteContextLookupFailuresTotal.WithLabelValues(admissionTestModel, "transport")
	before := testutil.ToFloat64(transportFailures)
	start := time.Now()
	resolved, err = newRouteContextClient(server.URL).Lookup(context.Background(), "tk_test", admissionTestModel)
	assertAdmittedWithoutDecision(t, resolved, err)
	if elapsed := time.Since(start); elapsed < routeContextLookupTimeout || elapsed > 3*routeContextLookupTimeout {
		t.Fatalf("lookup deadline not enforced: %v", time.Since(start))
	}
	if testutil.ToFloat64(transportFailures) != before+1 {
		t.Fatal("timeout not counted as a transport failure")
	}
}

func TestRouteContextMissingInputDoesNotFailOpen(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newRouteContextClient(server.URL)
	for _, tc := range []struct {
		key, model string
		status     int
	}{
		{"", "", http.StatusUnauthorized},
		{"", admissionTestModel, http.StatusUnauthorized},
		{"tk_test", "", http.StatusBadRequest},
	} {
		before := lookupFailures(t, tc.model)
		_, err := client.Lookup(context.Background(), tc.key, tc.model)
		if err == nil || err.apiError.Status != tc.status {
			t.Fatalf("key present=%v model=%q: %v", tc.key != "", tc.model, err)
		}
		if lookupFailures(t, tc.model) != before {
			t.Fatal("missing input counted as a control-plane outage")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("missing input reached the control plane")
	}
}
