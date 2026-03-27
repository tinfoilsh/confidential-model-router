package codeinterpreter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type sandboxControlplane interface {
	CreateSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error)
	GetSandbox(ctx context.Context, sandboxID string) (*SandboxInfo, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
}

type sandboxBootstrapper interface {
	Bootstrap(ctx context.Context, info *SandboxInfo, sourceRepo string) (*Sandbox, error)
}

// SandboxCred identifies an existing sandbox and provides its claim token,
// allowing the manager to connect without re-provisioning or re-claiming.
type SandboxCred struct {
	ID    string // sandbox_id
	Token string // claim token from a previous Claim call
}

type SandboxManager struct {
	spec         SandboxSpec
	execTimeout  time.Duration
	cred         SandboxCred // zero value = auto-provision
	controlplane sandboxControlplane
	bootstrapper sandboxBootstrapper

	mu      sync.Mutex
	sandbox *Sandbox
}

// NewSandboxManager creates a SandboxManager. If cred is provided, the manager
// attests the sandbox and connects using the pre-existing claim token instead of
// auto-provisioning a new one.
func NewSandboxManager(spec SandboxSpec, execTimeout time.Duration, controlplane sandboxControlplane, bootstrapper sandboxBootstrapper, cred ...SandboxCred) *SandboxManager {
	var c SandboxCred
	if len(cred) > 0 {
		c = cred[0]
	}
	return &SandboxManager{
		spec:         spec,
		execTimeout:  execTimeout,
		cred:         c,
		controlplane: controlplane,
		bootstrapper: bootstrapper,
	}
}

// GetSandbox returns the initialised Sandbox, lazily provisioning if needed.
func (m *SandboxManager) GetSandbox(ctx context.Context) (*Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.sandbox != nil {
		return m.sandbox, nil
	}

	if m.controlplane == nil {
		return nil, fmt.Errorf("sandbox controlplane client is not configured")
	}
	if m.bootstrapper == nil {
		return nil, fmt.Errorf("sandbox bootstrapper is not configured")
	}

	var (
		info *SandboxInfo
		err  error
	)

	var sandbox *Sandbox
	if strings.TrimSpace(m.cred.ID) != "" {
		// Existing sandbox: attest and connect using the pre-existing claim token.
		info, err = m.controlplane.GetSandbox(ctx, m.cred.ID)
		if err != nil {
			return nil, err
		}
		client, err := NewClient("https://"+info.Domain, m.spec.SourceRepo, m.execTimeout)
		if err != nil {
			return nil, fmt.Errorf("attest sandbox %s: %w", info.Domain, err)
		}
		sandbox = &Sandbox{ID: info.ID, client: client, token: m.cred.Token}
	} else {
		// Auto-provision: create via controlplane then bootstrap (attest + claim).
		if strings.TrimSpace(m.spec.Workload) == "" || strings.TrimSpace(m.spec.SourceRepo) == "" {
			return nil, fmt.Errorf("sandbox workload and source repo are required")
		}
		info, err = m.controlplane.CreateSandbox(ctx, m.spec)
		if err != nil {
			return nil, err
		}
		sandbox, err = m.bootstrapper.Bootstrap(ctx, info, m.spec.SourceRepo)
		if err != nil {
			_ = m.controlplane.DeleteSandbox(context.Background(), info.ID)
			return nil, err
		}
	}

	m.sandbox = sandbox
	return m.sandbox, nil
}

// Close deletes auto-provisioned sandboxes from the controlplane.
func (m *SandboxManager) Close(ctx context.Context) error {
	if m == nil || m.controlplane == nil {
		return nil
	}
	if strings.TrimSpace(m.cred.ID) != "" {
		return nil
	}

	m.mu.Lock()
	sandbox := m.sandbox
	m.mu.Unlock()
	if sandbox == nil || strings.TrimSpace(sandbox.ID) == "" {
		return nil
	}

	if err := m.controlplane.DeleteSandbox(ctx, sandbox.ID); err != nil {
		return err
	}

	m.mu.Lock()
	m.sandbox = nil
	m.mu.Unlock()
	return nil
}
