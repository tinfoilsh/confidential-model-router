package codeinterpreter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fixedControlplane is a mock sandboxControlplane that returns a pre-set domain.
type fixedControlplane struct {
	domain string
}

func (f *fixedControlplane) CreateSandbox(_ context.Context, _ SandboxSpec) (*SandboxInfo, error) {
	return &SandboxInfo{
		ID:        "mock-" + f.domain,
		Domain:    f.domain,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}, nil
}

func (f *fixedControlplane) GetSandbox(_ context.Context, sandboxID string) (*SandboxInfo, error) {
	return &SandboxInfo{
		ID:        sandboxID,
		Domain:    f.domain,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}, nil
}

func (f *fixedControlplane) DeleteSandbox(_ context.Context, _ string) error {
	return nil
}

// errControlplane always returns an error from CreateSandbox.
type errControlplane struct {
	err error
}

func (e *errControlplane) CreateSandbox(_ context.Context, _ SandboxSpec) (*SandboxInfo, error) {
	return nil, e.err
}

func (e *errControlplane) GetSandbox(_ context.Context, _ string) (*SandboxInfo, error) {
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

func (c *countingControlplane) CreateSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return c.fixedControlplane.CreateSandbox(ctx, spec)
}

func (c *countingControlplane) GetSandbox(ctx context.Context, sandboxID string) (*SandboxInfo, error) {
	return c.fixedControlplane.GetSandbox(ctx, sandboxID)
}

func (c *countingControlplane) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.fixedControlplane.DeleteSandbox(ctx, sandboxID)
}

func (c *countingControlplane) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// stubBootstrapper returns a Sandbox backed by a test HTTP server.
type stubBootstrapper struct {
	server *httptest.Server
	mu     sync.Mutex
	count  int
}

func newStubBootstrapper(t *testing.T) *stubBootstrapper {
	t.Helper()
	bootstrapper := &stubBootstrapper{}
	bootstrapper.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/contexts":
			bootstrapper.mu.Lock()
			bootstrapper.count++
			contextID := fmt.Sprintf("ctx-%d", bootstrapper.count)
			bootstrapper.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"%s"}`, contextID)))
		case "/execute":
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"stdout","text":"2\n"}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(bootstrapper.server.Close)
	return bootstrapper
}

func (s *stubBootstrapper) Bootstrap(_ context.Context, info *SandboxInfo, _ string) (*Sandbox, error) {
	client := &Client{
		baseURL:     s.server.URL,
		httpClient:  s.server.Client(),
		execTimeout: 10 * time.Second,
	}
	return &Sandbox{ID: info.ID, client: client, token: "test-token"}, nil
}

func (s *stubBootstrapper) contextCreateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func newTestSpec() SandboxSpec {
	return SandboxSpec{
		Workload:   sandboxWorkloadCodeInterpreter,
		SourceRepo: "test/repo",
	}
}

func TestSandboxManagerCreationFailure(t *testing.T) {
	t.Parallel()

	mgr := NewSandboxManager(
		newTestSpec(),
		10*time.Second,
		&errControlplane{err: errors.New("controlplane unavailable")},
		newStubBootstrapper(t),
	)
	defer mgr.Close(context.Background())

	_, err := mgr.GetSandbox(context.Background())
	if err == nil {
		t.Fatal("expected error from GetSandbox, got nil")
	}
}

func TestSandboxManagerBootstrapFailure(t *testing.T) {
	t.Parallel()

	fcp := &fixedControlplane{domain: "sandbox.test.local"}
	mgr := NewSandboxManager(
		newTestSpec(),
		10*time.Second,
		fcp,
		&errBootstrapper{err: errors.New("attestation failed")},
	)
	defer mgr.Close(context.Background())

	_, err := mgr.GetSandbox(context.Background())
	if err == nil {
		t.Fatal("expected error from GetSandbox, got nil")
	}
}

// errBootstrapper always returns an error from Bootstrap.
type errBootstrapper struct {
	err error
}

func (e *errBootstrapper) Bootstrap(_ context.Context, _ *SandboxInfo, _ string) (*Sandbox, error) {
	return nil, e.err
}

func TestSandboxManagerConcurrentExecute(t *testing.T) {
	t.Parallel()

	domain := "sandbox.test.local"
	cp := &countingControlplane{fixedControlplane: fixedControlplane{domain: domain}}
	bootstrapper := newStubBootstrapper(t)
	mgr := NewSandboxManager(
		newTestSpec(),
		10*time.Second,
		cp,
		bootstrapper,
	)
	defer mgr.Close(context.Background())

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			sandbox, err := mgr.GetSandbox(context.Background())
			if err != nil {
				t.Errorf("GetSandbox returned unexpected error: %v", err)
				return
			}
			result, err := sandbox.Execute(context.Background(), "call-concurrent", Args{Code: "print(1)"})
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

	if count := cp.getCount(); count != 1 {
		t.Errorf("expected exactly one sandbox creation, got %d", count)
	}
	if count := bootstrapper.contextCreateCount(); count != 1 {
		t.Errorf("expected exactly one context creation, got %d", count)
	}
}

func TestSandboxManagerContextCancellation(t *testing.T) {
	t.Parallel()

	// A controlplane that blocks until the context is cancelled.
	blockCh := make(chan struct{})
	blocking := &blockingControlplane{blockCh: blockCh}

	mgr := NewSandboxManager(
		newTestSpec(),
		10*time.Second,
		blocking,
		newStubBootstrapper(t),
	)
	defer mgr.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := mgr.GetSandbox(ctx)
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
}

// blockingControlplane blocks CreateSandbox until blockCh is closed or ctx is cancelled.
type blockingControlplane struct {
	blockCh chan struct{}
}

func (b *blockingControlplane) CreateSandbox(ctx context.Context, _ SandboxSpec) (*SandboxInfo, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.blockCh:
		return nil, errors.New("released")
	}
}

func (b *blockingControlplane) GetSandbox(_ context.Context, _ string) (*SandboxInfo, error) {
	return nil, errors.New("not implemented")
}

func (b *blockingControlplane) DeleteSandbox(_ context.Context, _ string) error {
	return nil
}
