package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteContextClientLookupCachesSuccess(t *testing.T) {
	priority := -1
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != routeContextPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, routeContextPath)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}

		var req routeContextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.APIKey != "tk_test" {
			t.Fatalf("request = %#v, want key", req)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(routeContext{
			Priority: &priority,
			OrgID:    "org_test",
		})
	}))
	defer server.Close()

	client := newRouteContextClient(server.URL)
	first, ok := client.Lookup(context.Background(), "tk_test", "gemma4-31b")
	if !ok {
		t.Fatal("expected first lookup to succeed")
	}
	second, ok := client.Lookup(context.Background(), "tk_test", "llama3-3-70b")
	if !ok {
		t.Fatal("expected second lookup to succeed from cache")
	}

	if first.Priority == nil || *first.Priority != priority {
		t.Fatalf("first context = %#v, want high priority", first)
	}
	if second.Priority == nil || *second.Priority != priority {
		t.Fatalf("second context = %#v, want high priority", second)
	}
	if first.OrgID != "org_test" || second.OrgID != "org_test" {
		t.Fatalf("contexts = (%#v, %#v), want org_test org", first, second)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestRouteContextClientLookupFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := newRouteContextClient(server.URL)
	if _, ok := client.Lookup(context.Background(), "tk_test", "gemma4-31b"); ok {
		t.Fatal("expected lookup to fail closed")
	}
}
