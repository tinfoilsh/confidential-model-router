package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tinfoilsh/confidential-model-router/manager"
)

const cachedAllowedResponse = `{"org_id":"org_test","priority":-2,"rate_limit":{"decision":"allowed","retry_after_seconds":0}}`
const cachedRejectedResponse = `{"org_id":"org_test","rate_limit":{"decision":"rejected","reason":"tokens","retry_after_seconds":30}}`

func TestRouteContextCachedDecisionsAndRecovery(t *testing.T) {
	var response atomic.Value
	response.Store(cachedAllowedResponse)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, response.Load().(string))
	}))
	defer server.Close()
	client := newRouteContextClient(server.URL)
	defer client.refreshes.Wait()

	got, err := client.Lookup(t.Context(), "tk_test", admissionTestModel)
	assertAdmittedWithoutDecision(t, got, err)
	client.refreshes.Wait()
	response.Store(cachedRejectedResponse)
	got, err = client.Lookup(t.Context(), "tk_test", admissionTestModel)
	if err != nil || got.OrgID != "org_test" || got.Priority == nil || *got.Priority != -2 || got.RateLimit.Decision != decisionAllowed {
		t.Fatalf("cached allow: %+v, %v", got, err)
	}
	client.refreshes.Wait()
	response.Store(cachedAllowedResponse)
	_, err = client.Lookup(t.Context(), "tk_test", admissionTestModel)
	if err == nil || err.apiError.Status != http.StatusTooManyRequests || err.retryAfter != "30" {
		t.Fatalf("cached rejection: %v", err)
	}
	client.refreshes.Wait()
	got, err = client.Lookup(t.Context(), "tk_test", admissionTestModel)
	if err != nil || got.RateLimit.Decision != decisionAllowed {
		t.Fatalf("recovery: %+v, %v", got, err)
	}
	client.refreshes.Wait()
	if calls.Load() != 4 {
		t.Fatalf("refresh calls = %d, want one per lookup", calls.Load())
	}
}

func TestRouteContextRefreshFailureKeepsCachedStatus(t *testing.T) {
	for _, initial := range []string{cachedAllowedResponse, cachedRejectedResponse} {
		for _, failure := range []struct {
			name   string
			status int
			body   string
		}{
			{"unavailable", http.StatusServiceUnavailable, `{}`},
			{"invalid JSON", http.StatusOK, `{`},
			{"invalid decision", http.StatusOK, `{"rate_limit":{"decision":"unknown"}}`},
			{"oversized", http.StatusOK, strings.Repeat(" ", routeContextResponseLimit+1)},
			{"timeout", 0, ""},
		} {
			t.Run(initial+failure.name, func(t *testing.T) {
				var fail atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !fail.Load() {
						io.WriteString(w, initial)
						return
					}
					if failure.status == 0 {
						io.Copy(io.Discard, r.Body)
						<-r.Context().Done()
						return
					}
					w.WriteHeader(failure.status)
					io.WriteString(w, failure.body)
				}))
				defer server.Close()
				client := newRouteContextClient(server.URL)
				defer client.refreshes.Wait()
				client.Lookup(t.Context(), "tk_test", admissionTestModel)
				client.refreshes.Wait()
				fail.Store(true)
				for range 2 {
					got, err := client.Lookup(t.Context(), "tk_test", admissionTestModel)
					if initial == cachedRejectedResponse {
						if err == nil || err.apiError.Status != http.StatusTooManyRequests {
							t.Fatalf("cached rejection lost: %+v %v", got, err)
						}
					} else if err != nil || got.Priority == nil || *got.Priority != -2 || got.OrgID != "org_test" {
						t.Fatalf("cached context lost: %+v %v", got, err)
					}
					client.refreshes.Wait()
				}
			})
		}
	}
}

func TestRouteContextCachedCredentialDenials(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var current atomic.Int64
			current.Store(int64(status))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(int(current.Load()))
				if current.Load() == http.StatusOK {
					io.WriteString(w, cachedAllowedResponse)
				} else {
					io.WriteString(w, quotaDenialTestBody)
				}
			}))
			defer server.Close()
			client := newRouteContextClient(server.URL)
			defer client.refreshes.Wait()
			got, err := client.Lookup(t.Context(), "tk_test", admissionTestModel)
			assertAdmittedWithoutDecision(t, got, err)
			client.refreshes.Wait()
			for _, next := range []int{http.StatusServiceUnavailable, http.StatusOK} {
				current.Store(int64(next))
				_, err = client.Lookup(t.Context(), "tk_test", admissionTestModel)
				if err == nil || err.apiError.Status != status {
					t.Fatalf("cached denial lost: %v", err)
				}
				if status == http.StatusTooManyRequests {
					rec := httptest.NewRecorder()
					err.write(rec)
					assertQuotaDenial(t, rec, "30")
				}
				client.refreshes.Wait()
			}
			got, err = client.Lookup(t.Context(), "tk_test", admissionTestModel)
			if err != nil || got.RateLimit == nil || got.RateLimit.Decision != decisionAllowed {
				t.Fatalf("denial did not recover: %+v %v", got, err)
			}
		})
	}
}

func TestRouteContextRefreshDoesNotBlockOrInheritCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-release:
			io.WriteString(w, cachedAllowedResponse)
		case <-r.Context().Done():
			t.Error("refresh canceled before response was released")
		}
	}))
	defer server.Close()
	client := newRouteContextClient(server.URL)
	defer client.refreshes.Wait()
	releaseResponse := sync.OnceFunc(func() { close(release) })
	defer releaseResponse()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got, err := client.Lookup(ctx, "tk_test", admissionTestModel)
	assertAdmittedWithoutDecision(t, got, err)
	cancel()
	select {
	case <-started:
	case <-time.After(5 * routeContextLookupTimeout):
		t.Fatal("background refresh did not reach the control plane")
	}
	releaseResponse()
	client.refreshes.Wait()
	got, err = client.cache.get(routeContextKey("tk_test", admissionTestModel))
	if err != nil || got.OrgID != "org_test" || got.RateLimit == nil {
		t.Fatalf("request cancellation prevented cache update: %+v %v", got, err)
	}
}

func TestRouteContextRefreshBoundAndConcurrentAccounting(t *testing.T) {
	const limit = 4
	started := make(chan struct{}, limit)
	release := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			io.WriteString(w, cachedAllowedResponse)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	client := newRouteContextClient(server.URL)
	client.refreshSlots = make(chan struct{}, limit)
	defer client.refreshes.Wait()
	releaseResponses := sync.OnceFunc(func() { close(release) })
	defer releaseResponses()
	var wg sync.WaitGroup
	for range limit {
		wg.Go(func() {
			got, err := client.Lookup(t.Context(), "tk_test", admissionTestModel)
			assertAdmittedWithoutDecision(t, got, err)
		})
	}
	wg.Wait()
	for range limit {
		select {
		case <-started:
		case <-time.After(5 * routeContextLookupTimeout):
			t.Fatal("concurrent requests were coalesced or blocked")
		}
	}
	busy := manager.RouteContextLookupFailuresTotal.WithLabelValues(admissionTestModel, "busy")
	before := testutil.ToFloat64(busy)
	got, err := client.Lookup(t.Context(), "tk_other", admissionTestModel)
	assertAdmittedWithoutDecision(t, got, err)
	if calls.Load() != limit || testutil.ToFloat64(busy) != before+1 {
		t.Fatalf("unbounded refreshes: calls=%d", calls.Load())
	}
	releaseResponses()
	client.refreshes.Wait()
	got, err = client.cache.get(routeContextKey("tk_test", admissionTestModel))
	if err != nil || got.RateLimit == nil {
		t.Fatal("concurrent refreshes did not populate cache")
	}
}

func TestRouteContextCacheAccountsForResizedDenials(t *testing.T) {
	cache := routeContextCache{maxBytes: 4 * routeContextCacheEntryOverhead}
	key := routeContextKey("tk_test", admissionTestModel)
	denial := &routeContextError{apiError: &manager.APIError{
		Status:  http.StatusTooManyRequests,
		Message: strings.Repeat("x", routeContextCacheEntryOverhead),
	}, retryAfter: "30"}
	cache.put(key, routeContext{}, denial, 1)
	want := routeContextCacheEntryOverhead + routeContextCacheStringOverhead*(len(denial.apiError.Message)+len(denial.retryAfter))
	if cache.bytes != want {
		t.Fatalf("denial cost = %d, want %d", cache.bytes, want)
	}
	cache.put(key, routeContext{}, nil, 2)
	if cache.bytes != routeContextCacheEntryOverhead || cache.lru.Len() != 1 {
		t.Fatalf("shrinking entry retained cost: %d", cache.bytes)
	}
	cache.put(key, routeContext{}, denial, 3)
	if cache.bytes != want || cache.lru.Len() != 1 {
		t.Fatalf("growing entry cost = %d, want %d", cache.bytes, want)
	}
}

func TestRouteContextCacheIsolationEvictionAndOrdering(t *testing.T) {
	const org = "org_test"
	entryBytes := routeContextCacheEntryOverhead + routeContextCacheStringOverhead*len(org)
	cache := routeContextCache{maxBytes: 2 * entryBytes}
	first := routeContextKey("tk_a", admissionTestModel)
	second := routeContextKey("tk_b", admissionTestModel)
	third := routeContextKey("tk_a", "other-model")
	for _, key := range []routeContextCacheKey{first, second} {
		cache.put(key, routeContext{OrgID: org}, nil, 1)
	}
	if got, _ := cache.get(third); got.OrgID != "" {
		t.Fatal("cache leaked across models")
	}
	if got, _ := cache.get(routeContextKey("tk_other", admissionTestModel)); got.OrgID != "" {
		t.Fatal("cache leaked across credentials")
	}
	cache.get(first)
	cache.put(third, routeContext{OrgID: org}, nil, 2)
	if got, _ := cache.get(second); got.OrgID != "" {
		t.Fatal("least recently used entry not evicted")
	}
	cache.put(first, routeContext{OrgID: "new"}, nil, 3)
	cache.put(first, routeContext{OrgID: "old"}, nil, 2)
	if got, _ := cache.get(first); got.OrgID != "new" {
		t.Fatalf("late response overwrote newer context: %+v", got)
	}
	cache.put(third, routeContext{OrgID: strings.Repeat("x", cache.maxBytes)}, nil, 4)
	if got, _ := cache.get(third); got.OrgID != org {
		t.Fatal("oversized entry displaced cached context")
	}
	for i := range 1000 {
		cache.put(routeContextKey(fmt.Sprint(i), admissionTestModel), routeContext{OrgID: org}, nil, uint64(i))
		if cache.bytes > cache.maxBytes || len(cache.entries) > 2 || cache.lru.Len() != len(cache.entries) {
			t.Fatalf("cache exceeded budget: %d bytes, %d entries", cache.bytes, len(cache.entries))
		}
	}
}

func TestRouteContextCacheLateRefreshAfterEviction(t *testing.T) {
	cache := routeContextCache{maxBytes: 2 * routeContextCacheEntryOverhead}
	key := routeContextKey("tk_test", admissionTestModel)
	other := routeContextKey("tk_other", admissionTestModel)
	newer := routeContext{OrgID: "new"}
	cache.put(key, newer, nil, 2)
	cache.put(other, routeContext{OrgID: "other"}, nil, 3)
	if got, _ := cache.get(key); got.OrgID != "" {
		t.Fatal("fixture did not evict newer result")
	}
	cache.put(key, routeContext{OrgID: "old"}, nil, 1)
	if got, _ := cache.get(key); got.OrgID != "" {
		t.Fatalf("late refresh revived an evicted result: %+v", got)
	}
	if got, _ := cache.get(other); got.OrgID != "other" {
		t.Fatal("late refresh displaced an unrelated cached result")
	}
	cache.put(key, newer, nil, 4)
	if got, _ := cache.get(key); got.OrgID != newer.OrgID {
		t.Fatal("fresh refresh could not repopulate evicted key")
	}
}
