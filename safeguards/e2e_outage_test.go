package safeguards

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestE2E_RouterRespondsWhileSidecarHangs(t *testing.T) {
	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Close only returns once handlers exit, so release them first.
	defer hanging.Close()
	defer close(release)

	s := NewSubmitter(hanging.URL)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w, capture, finish := s.Observe(w, r, isChatToken)
		defer finish()
		capture.SetMessages([]Message{{Role: "user", Content: "hi"}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	})
	router := httptest.NewServer(handler)
	defer router.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < maxPending+maxInFlight+20; i++ {
		req, _ := http.NewRequest(http.MethodPost, router.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer chat-jwt")
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d failed while sidecar hung: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || time.Since(start) > time.Second {
			t.Fatalf("request %d degraded: status=%d elapsed=%s", i, resp.StatusCode, time.Since(start))
		}
	}

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(drainTimeout + 5*time.Second):
		t.Fatal("Close did not return while sidecar hung")
	}
}

func TestE2E_RouterRespondsWhenSidecarUnreachableOrRejecting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := "http://" + listener.Addr().String()
	listener.Close()

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusInternalServerError)
	}))
	defer rejecting.Close()

	for name, url := range map[string]string{"unreachable": unreachable, "rejecting": rejecting.URL} {
		t.Run(name, func(t *testing.T) {
			s := NewSubmitter(url)
			defer s.Close()
			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w, capture, finish := s.Observe(w, r, isChatToken)
				defer finish()
				capture.SetMessages([]Message{{Role: "user", Content: "hi"}})
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			}))
			defer router.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, router.URL+"/v1/chat/completions", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer chat-jwt")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("client failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
		})
	}
}
