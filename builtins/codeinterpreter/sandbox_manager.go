package codeinterpreter

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type sandboxControlplane interface {
	CreateSandbox(ctx context.Context, spec SandboxSpec, session *Session, callerAPIKeyID string) (*Sandbox, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
}

type sandboxBootstrapper interface {
	Bootstrap(ctx context.Context, sandbox *Sandbox, sourceRepo string) (*SandboxGateway, error)
}

type SandboxManager struct {
	spec           SandboxSpec
	session        *Session
	callerAPIKeyID string
	controlplane   sandboxControlplane
	bootstrapper   sandboxBootstrapper

	mu      sync.Mutex
	sandbox *Sandbox
	gateway *SandboxGateway
}

func NewSandboxManager(spec SandboxSpec, session *Session, callerAPIKeyID string, controlplane sandboxControlplane, bootstrapper sandboxBootstrapper) *SandboxManager {
	return &SandboxManager{
		spec:           spec,
		session:        session,
		callerAPIKeyID: strings.TrimSpace(callerAPIKeyID),
		controlplane:   controlplane,
		bootstrapper:   bootstrapper,
	}
}

func (m *SandboxManager) Execute(ctx context.Context, callID, rawArgs string) (Result, error) {
	if err := m.ensure(ctx); err != nil {
		return failedResult(callID, nil, "", err), nil
	}
	return m.gateway.Execute(ctx, callID, rawArgs, m.sandbox.ID)
}

func (m *SandboxManager) ensure(ctx context.Context) error {
	m.mu.Lock()
	if m.gateway != nil && m.sandbox != nil {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	if m.controlplane == nil {
		return fmt.Errorf("sandbox controlplane client is not configured")
	}
	if m.bootstrapper == nil {
		return fmt.Errorf("sandbox bootstrapper is not configured")
	}
	if m.session == nil || !m.session.Managed {
		return fmt.Errorf("managed sandbox session is required")
	}
	if strings.TrimSpace(m.spec.Image) == "" || strings.TrimSpace(m.spec.SourceRepo) == "" {
		return fmt.Errorf("sandbox image and source repo are required")
	}

	sandbox, err := m.controlplane.CreateSandbox(ctx, m.spec, m.session, m.callerAPIKeyID)
	if err != nil {
		return err
	}

	gateway, err := m.bootstrapper.Bootstrap(ctx, sandbox, m.spec.SourceRepo)
	if err != nil {
		_ = m.controlplane.DeleteSandbox(context.Background(), sandbox.ID)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gateway == nil {
		m.sandbox = sandbox
		m.gateway = gateway
	}
	return nil
}

func (m *SandboxManager) Close(ctx context.Context) error {
	if m == nil || m.controlplane == nil {
		return nil
	}

	m.mu.Lock()
	sandbox := m.sandbox
	m.mu.Unlock()
	if sandbox == nil || strings.TrimSpace(sandbox.ID) == "" {
		return nil
	}

	return m.controlplane.DeleteSandbox(ctx, sandbox.ID)
}
