package codeinterpreter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// errControlplane always returns an error from CreateSandbox.
type errControlplane struct {
	err error
}

func (e *errControlplane) CreateSandbox(_ context.Context, _ SandboxSpec, _ *Session, _ string) (*Sandbox, error) {
	return nil, e.err
}

func (e *errControlplane) DeleteSandbox(_ context.Context, _ string) error {
	return nil
}

// countingControlplane wraps fixedControlplane and tracks CreateSandbox calls.
type countingControlplane struct {
	fixedControlplane
	mu    sync.Mutex
	count int
}

func (c *countingControlplane) CreateSandbox(ctx context.Context, spec SandboxSpec, session *Session, callerAPIKeyID string) (*Sandbox, error) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return c.fixedControlplane.CreateSandbox(ctx, spec, session, callerAPIKeyID)
}

func (c *countingControlplane) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// stubBootstrapper returns a SandboxGateway backed by a test HTTP server.
type stubBootstrapper struct {
	server *httptest.Server
}

func newStubBootstrapper(t *testing.T) *stubBootstrapper {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"stdout","text":"2\n"}` + "\n"))
	}))
	t.Cleanup(server.Close)
	return &stubBootstrapper{server: server}
}

func (s *stubBootstrapper) Bootstrap(_ context.Context, sandbox *Sandbox, _ string) (*SandboxGateway, error) {
	client := &Client{
		baseURL:     s.server.URL,
		httpClient:  s.server.Client(),
		execTimeout: 10 * time.Second,
	}
	return &SandboxGateway{client: client, runtimeCredential: "test-token"}, nil
}

func newTestSpec() SandboxSpec {
	return SandboxSpec{
		Workload:   sandboxWorkloadCodeInterpreter,
		Image:      "ghcr.io/test/img:latest",
		SourceRepo: "test/repo",
	}
}

func newManagedSession() *Session {
	return &Session{Managed: true, TTLSeconds: 300}
}

func TestSandboxManagerCreationFailure(t *testing.T) {
	t.Parallel()

	mgr := NewSandboxManager(
		newTestSpec(),
		newManagedSession(),
		"",
		&errControlplane{err: errors.New("controlplane unavailable")},
		newStubBootstrapper(t),
	)
	defer mgr.Close(context.Background())

	result, err := mgr.Execute(context.Background(), "call-1", `{"code": "print(1+1)"}`)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("expected status %q, got %q", StatusFailed, result.Status)
	}
}

func TestSandboxManagerBootstrapFailure(t *testing.T) {
	t.Parallel()

	fcp := &fixedControlplane{domain: "sandbox.test.local"}
	mgr := NewSandboxManager(
		newTestSpec(),
		newManagedSession(),
		"",
		fcp,
		&errBootstrapper{err: errors.New("attestation failed")},
	)
	defer mgr.Close(context.Background())

	result, err := mgr.Execute(context.Background(), "call-1", `{"code": "print(1+1)"}`)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("expected status %q, got %q", StatusFailed, result.Status)
	}
}

// errBootstrapper always returns an error from Bootstrap.
type errBootstrapper struct {
	err error
}

func (e *errBootstrapper) Bootstrap(_ context.Context, _ *Sandbox, _ string) (*SandboxGateway, error) {
	return nil, e.err
}

func TestSandboxManagerConcurrentExecute(t *testing.T) {
	t.Parallel()

	domain := "sandbox.test.local"
	cp := &countingControlplane{fixedControlplane: fixedControlplane{domain: domain}}
	mgr := NewSandboxManager(
		newTestSpec(),
		newManagedSession(),
		"",
		cp,
		newStubBootstrapper(t),
	)
	defer mgr.Close(context.Background())

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			result, err := mgr.Execute(context.Background(), "call-concurrent", `{"code": "print(1)"}`)
			if err != nil {
				t.Errorf("Execute returned unexpected error: %v", err)
				return
			}
			if result.Status != StatusCompleted {
				t.Errorf("expected status %q, got %q", StatusCompleted, result.Status)
			}
		}()
	}
	wg.Wait()

	// The sandbox should have been created at most once per successful bootstrap.
	// Due to the DCL pattern, a small number of concurrent creations is possible,
	// but it must not be equal to goroutines (which would indicate no deduplication).
	if count := cp.getCount(); count == goroutines {
		t.Errorf("expected sandbox creation to be deduplicated, but CreateSandbox was called %d times", count)
	}
}

func TestSandboxManagerContextCancellation(t *testing.T) {
	t.Parallel()

	// A controlplane that blocks until the context is cancelled.
	blockCh := make(chan struct{})
	blocking := &blockingControlplane{blockCh: blockCh}

	mgr := NewSandboxManager(
		newTestSpec(),
		newManagedSession(),
		"",
		blocking,
		newStubBootstrapper(t),
	)
	defer mgr.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := mgr.Execute(ctx, "call-1", `{"code": "print(1)"}`)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("expected status %q after context cancellation, got %q", StatusFailed, result.Status)
	}
}

// blockingControlplane blocks CreateSandbox until blockCh is closed or ctx is cancelled.
type blockingControlplane struct {
	blockCh chan struct{}
}

func (b *blockingControlplane) CreateSandbox(ctx context.Context, _ SandboxSpec, _ *Session, _ string) (*Sandbox, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.blockCh:
		return nil, errors.New("released")
	}
}

func (b *blockingControlplane) DeleteSandbox(_ context.Context, _ string) error {
	return nil
}
