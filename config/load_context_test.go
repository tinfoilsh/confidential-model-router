package config

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoadContextCancelsStalledResponseBody(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		// Keep the body stalled even after the server sees cancellation.
		// Ending the body here races the client read and can return clean EOF.
		<-release
	}))
	defer server.Close()
	defer close(release)
	original := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = original })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := LoadContext(ctx, server.URL, false)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stalled config fetch returned %v, want cancellation", err)
	}
}
