package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestRouteContextMetadataCoalescesColdRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const callers = 16
		var calls atomic.Int64
		release := make(chan struct{})
		client := newRouteContextClient("https://controlplane.invalid")
		client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"org_id":"org_test"}`))}, nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		canceled := make(chan routeContext, 1)
		go func() {
			resolved, _ := client.Metadata(ctx, "tk_test")
			canceled <- resolved
		}()
		synctest.Wait()
		results := make(chan routeContext, callers)
		for range callers {
			go func() {
				resolved, err := client.Metadata(t.Context(), "tk_test")
				if err != nil {
					t.Error(err)
				}
				results <- resolved
			}()
		}
		synctest.Wait()
		cancel()
		if resolved := <-canceled; !resolved.unavailable {
			t.Fatalf("canceled caller received metadata: %+v", resolved)
		}
		close(release)
		for range callers {
			if resolved := <-results; resolved.OrgID != "org_test" || resolved.unavailable {
				t.Fatalf("shared lookup lost identity: %+v", resolved)
			}
		}
		client.Metadata(t.Context(), "tk_test")
		if calls.Load() != 1 {
			t.Fatalf("metadata requests = %d, want 1", calls.Load())
		}
	})
}

func TestRouteContextMetadataRefreshesWithoutBlocking(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantOrg string
		status, wantError   int
		initiallyEmpty      bool
	}{
		{name: "newly eligible organization", initiallyEmpty: true, status: http.StatusOK, body: `{"org_id":"org_new"}`, wantOrg: "org_new"},
		{name: "organization changed", status: http.StatusOK, body: `{"org_id":"org_new"}`, wantOrg: "org_new"},
		{name: "organization removed", status: http.StatusOK, body: `{}`},
		{name: "credential denied", status: http.StatusForbidden, wantError: http.StatusForbidden},
		{name: "quota denied", status: http.StatusTooManyRequests, wantError: http.StatusTooManyRequests},
		{name: "outage", status: http.StatusServiceUnavailable, wantOrg: "org_test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int64
				release := make(chan struct{})
				initialOrg := "org_test"
				if tc.initiallyEmpty {
					initialOrg = ""
				}
				client := newRouteContextClient("https://controlplane.invalid")
				client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					status, body := http.StatusOK, fmt.Sprintf(`{"org_id":%q}`, initialOrg)
					if calls.Add(1) != 1 {
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
						status, body = tc.status, tc.body
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				client.Metadata(t.Context(), "tk_test")
				time.Sleep(routeMetadataRefreshTTL)
				started := time.Now()
				for range 3 {
					resolved, err := client.Metadata(t.Context(), "tk_test")
					if err != nil || resolved.OrgID != initialOrg || resolved.unavailable {
						t.Fatalf("stale metadata lost: %+v, %v", resolved, err)
					}
				}
				synctest.Wait()
				if time.Since(started) != 0 || calls.Load() != 2 {
					t.Fatalf("warm requests waited or duplicated refreshes: duration=%s calls=%d", time.Since(started), calls.Load())
				}
				close(release)
				synctest.Wait()
				resolved, err := client.Metadata(t.Context(), "tk_test")
				if tc.wantError != 0 {
					if err == nil || err.apiError.Status != tc.wantError {
						t.Fatalf("credential denial lost: %v", err)
					}
				} else if err != nil || resolved.OrgID != tc.wantOrg || resolved.unavailable {
					t.Fatalf("refreshed metadata = %+v, %v", resolved, err)
				}
				if calls.Load() != 2 {
					t.Fatalf("metadata calls = %d, want 2", calls.Load())
				}
			})
		})
	}
}

func TestRouteContextMetadataRespectsRefreshLimit(t *testing.T) {
	client := newRouteContextClient("https://controlplane.invalid")
	client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("metadata exceeded the concurrent lookup limit")
		return nil, fmt.Errorf("unexpected request")
	})
	for range cap(client.refreshSlots) {
		client.refreshSlots <- struct{}{}
	}
	resolved, err := client.Metadata(t.Context(), "tk_test")
	if err != nil || !resolved.unavailable {
		t.Fatalf("saturated metadata lookup = %+v, %v", resolved, err)
	}
}

func TestRouteContextMetadataKeepsNewerAdmissionIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		client := newRouteContextClient("https://controlplane.invalid")
		client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			response := `{"org_id":"org_new","rate_limit":{"decision":"allowed","retry_after_seconds":0}}`
			if !strings.Contains(string(body), `"model"`) {
				<-release
				response = `{"org_id":"org_old"}`
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
		})
		go client.Metadata(t.Context(), "tk_test")
		synctest.Wait()
		client.Lookup(t.Context(), "tk_test", admissionTestModel)
		client.refreshes.Wait()
		close(release)
		synctest.Wait()
		resolved, err := client.Metadata(t.Context(), "tk_test")
		if err != nil || resolved.OrgID != "org_new" || resolved.RateLimit != nil {
			t.Fatalf("late metadata replaced admission identity: %+v, %v", resolved, err)
		}
		admission, _ := client.cache.get(routeContextKey("tk_test", admissionTestModel))
		if admission.RateLimit == nil || admission.OrgID != "org_new" {
			t.Fatalf("metadata replaced the admission decision: %+v", admission)
		}
	})
}

func TestRouteContextMetadataDoesNotShareModelDenials(t *testing.T) {
	client := newRouteContextClient("https://controlplane.invalid")
	client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		status, response := http.StatusOK, `{"org_id":"org_test"}`
		if strings.Contains(string(body), `"model"`) {
			status, response = http.StatusForbidden, `{}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	client.Metadata(t.Context(), "tk_test")
	client.Lookup(t.Context(), "tk_test", admissionTestModel)
	client.refreshes.Wait()
	resolved, err := client.Metadata(t.Context(), "tk_test")
	if err != nil || resolved.OrgID != "org_test" {
		t.Fatalf("model denial contaminated organization metadata: %+v, %v", resolved, err)
	}
	_, err = client.cache.get(routeContextKey("tk_test", admissionTestModel))
	if err == nil || err.apiError.Status != http.StatusForbidden {
		t.Fatalf("model denial was not retained: %v", err)
	}
}

func BenchmarkRouteContextCachedMetadata(b *testing.B) {
	client := newRouteContextClient("https://controlplane.invalid")
	var calls atomic.Int64
	client.httpClient.Transport = routeContextRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"org_id":"org_test"}`))}, nil
	})
	client.Metadata(b.Context(), "tk_test")
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resolved, err := client.Metadata(b.Context(), "tk_test")
			if err != nil || resolved.OrgID != "org_test" {
				b.Error("cached metadata lost")
				return
			}
		}
	})
	b.StopTimer()
	if calls.Load() != 1 {
		b.Fatalf("warm cache made %d additional HTTP requests", calls.Load()-1)
	}
}
