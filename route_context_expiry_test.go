package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type routeContextRoundTripFunc func(*http.Request) (*http.Response, error)

func (f routeContextRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRouteContextCachedRejectionCountdown(t *testing.T) {
	const retrySeconds int64 = 58
	const responseDelay = routeContextLookupTimeout / 2
	for _, reason := range []string{rateReasonRequests, rateReasonTokens} {
		for _, tc := range []struct {
			name        string
			retry       int64
			delay, age  time.Duration
			wantSeconds int64
		}{
			{"fresh", retrySeconds, 0, 0, retrySeconds},
			{"elapsed seconds", retrySeconds, 0, 40 * time.Second, 18},
			{"round up", retrySeconds, 0, 40*time.Second + time.Nanosecond, 18},
			{"last nanosecond", retrySeconds, 0, time.Duration(retrySeconds)*time.Second - time.Nanosecond, 1},
			{"at expiry", retrySeconds, 0, time.Duration(retrySeconds) * time.Second, 0},
			{"after expiry", retrySeconds, 0, 2 * time.Minute, 0},
			{"response latency counts", 1, responseDelay, time.Second - responseDelay, 0},
			{"large retry does not overflow", math.MaxInt64, 0, time.Second, math.MaxInt64 - 1},
		} {
			t.Run(reason+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					client := newRouteContextClient("https://controlplane.invalid")
					calls := 0
					client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						calls++
						status, body := http.StatusServiceUnavailable, `{}`
						if calls == 1 {
							time.Sleep(tc.delay)
							status = http.StatusOK
							body = fmt.Sprintf(`{"org_id":"org_test","priority":-2,"rate_limit":{"decision":"rejected","reason":%q,"retry_after_seconds":%d}}`, reason, tc.retry)
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
					})
					got, err := client.Lookup(t.Context(), "tk_test", admissionTestModel)
					assertAdmittedWithoutDecision(t, got, err)
					client.refreshes.Wait()
					key := routeContextKey("tk_test", admissionTestModel)
					snapshot, _ := client.cache.get(key)
					if snapshot.RateLimit == nil {
						t.Fatal("rejection was not cached")
					}
					time.Sleep(tc.age)
					for range 2 {
						got, err = client.Lookup(t.Context(), "tk_test", admissionTestModel)
						client.refreshes.Wait()
						if got.OrgID != "org_test" || got.Priority == nil || *got.Priority != -2 {
							t.Fatalf("cached routing metadata lost: %+v", got)
						}
						if tc.wantSeconds == 0 {
							if err != nil || got.RateLimit != nil {
								t.Fatalf("expired rejection still blocks: %+v %v", got, err)
							}
							continue
						}
						if err == nil || err.apiError.Status != http.StatusTooManyRequests {
							t.Fatalf("unexpired rejection lost: %+v %v", got, err)
						}
						rec := httptest.NewRecorder()
						err.write(rec)
						want := strconv.FormatInt(tc.wantSeconds, 10)
						if rec.Header().Get("Retry-After") != want || !strings.Contains(rec.Body.String(), "Retry after "+want+" seconds.") || !strings.Contains(rec.Body.String(), "for "+reason) {
							t.Fatalf("stale retry response: %v %s", rec.Header(), rec.Body.String())
						}
					}
					if *snapshot.RateLimit.RetryAfterSeconds != tc.retry {
						t.Fatal("countdown mutated a previously returned snapshot")
					}
					if calls != 3 {
						t.Fatalf("refresh calls = %d, want one per lookup", calls)
					}
				})
			})
		}
	}
}

func TestRouteContextExpiredRejectionCanRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const retrySeconds int64 = 2
		client := newRouteContextClient("https://controlplane.invalid")
		client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := fmt.Sprintf(`{"rate_limit":{"decision":"rejected","reason":"requests","retry_after_seconds":%d}}`, retrySeconds)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		client.Lookup(t.Context(), "tk_test", admissionTestModel)
		client.refreshes.Wait()
		time.Sleep(time.Duration(retrySeconds) * time.Second)
		got, err := client.Lookup(t.Context(), "tk_test", admissionTestModel)
		assertAdmittedWithoutDecision(t, got, err)
		client.refreshes.Wait()
		_, err = client.Lookup(t.Context(), "tk_test", admissionTestModel)
		client.refreshes.Wait()
		if err == nil || err.retryAfter != strconv.FormatInt(retrySeconds, 10) {
			t.Fatalf("new window rejection not enforced: %v", err)
		}
	})
}

func TestRouteContextCredentialDenialsDoNotExpireWithModelWindow(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				response := &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"30"}}}
				denial := credentialDenial(response, []byte(quotaDenialTestBody))
				cache := routeContextCache{maxBytes: routeContextCacheMaxBytes}
				key := routeContextKey("tk_test", admissionTestModel)
				cache.put(key, routeContext{}, denial, 1)
				time.Sleep(2 * time.Minute)
				_, got := cache.get(key)
				if got != denial {
					t.Fatalf("credential or per-key budget denial expired: %v", got)
				}
			})
		})
	}
}
