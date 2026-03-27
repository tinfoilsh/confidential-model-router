package codeinterpreter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	StatusInProgress   = "in_progress"
	StatusInterpreting = "interpreting"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
)

// Result holds the outcome of a code execution call.
type Result struct {
	ID          string
	Code        string
	ContainerID string // = sandbox_id, never from LLM
	ContextID   string // kernel context within the sandbox
	Status      string
	Outputs     []Output
	ExitCode    int
}

// Sandbox is a stateful handle to a running sandbox enclave.
type Sandbox struct {
	ID        string
	client    *Client
	token     string
	mu        sync.Mutex
	contextID string // current kernel context, empty until first Execute
}

// Execute resolves the kernel context (hinted > existing > create) and runs code.
func (s *Sandbox) Execute(ctx context.Context, callID string, args Args) (Result, error) {
	contextID, err := s.resolveContextID(ctx, args.ContextID)
	if err != nil {
		return failedResult(callID, args.Code, err), nil
	}

	outputs, exitCode, err := s.client.Execute(ctx, contextID, args, s.token)
	if err != nil {
		return failedResult(callID, args.Code, err), nil
	}

	status := StatusCompleted
	if exitCode != 0 {
		status = StatusFailed
	}
	return Result{
		ID:          callID,
		Code:        args.Code,
		ContainerID: s.ID,
		ContextID:   contextID,
		Status:      status,
		Outputs:     outputs,
		ExitCode:    exitCode,
	}, nil
}

func (s *Sandbox) resolveContextID(ctx context.Context, hinted string) (string, error) {
	hinted = strings.TrimSpace(hinted)
	if hinted != "" {
		return hinted, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.contextID != "" {
		return s.contextID, nil
	}

	id, err := s.client.CreateContext(ctx, s.token)
	if err != nil {
		return "", err
	}
	s.contextID = id
	return s.contextID, nil
}

// SandboxBootstrapper attests a sandbox enclave and returns a ready Sandbox.
type SandboxBootstrapper struct {
	execTimeout time.Duration
}

func NewSandboxBootstrapper(execTimeout time.Duration) *SandboxBootstrapper {
	if execTimeout <= 0 {
		execTimeout = 60 * time.Second
	}
	return &SandboxBootstrapper{execTimeout: execTimeout}
}

// Bootstrap attests the enclave at info.Domain, claims it, and returns a Sandbox ready for use.
func (b *SandboxBootstrapper) Bootstrap(ctx context.Context, info *SandboxInfo, sourceRepo string) (*Sandbox, error) {
	if info == nil {
		return nil, fmt.Errorf("sandbox info is required")
	}
	if strings.TrimSpace(sourceRepo) == "" {
		return nil, fmt.Errorf("sandbox source repo is required for attestation")
	}

	client, err := NewClient("https://"+info.Domain, sourceRepo, b.execTimeout)
	if err != nil {
		return nil, fmt.Errorf("attest sandbox %s: %w", info.Domain, err)
	}

	token, err := client.Claim(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sandbox %s: %w", info.Domain, err)
	}

	return &Sandbox{
		ID:     info.ID,
		client: client,
		token:  token,
	}, nil
}

func failedResult(callID, code string, err error) Result {
	message := ""
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	outputs := []Output{}
	if message != "" {
		outputs = append(outputs, Output{Type: "logs", Logs: message})
	}
	return Result{
		ID:       callID,
		Code:     code,
		Status:   StatusFailed,
		Outputs:  outputs,
		ExitCode: -1,
	}
}
