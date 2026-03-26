package codeinterpreter

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type SandboxBootstrapper struct {
	execTimeout time.Duration
}

type SandboxGateway struct {
	client            *Client
	runtimeCredential string
}

func NewSandboxBootstrapper(execTimeout time.Duration) *SandboxBootstrapper {
	if execTimeout <= 0 {
		execTimeout = 60 * time.Second
	}
	return &SandboxBootstrapper{execTimeout: execTimeout}
}

func (b *SandboxBootstrapper) Bootstrap(ctx context.Context, sandbox *Sandbox, sourceRepo string) (*SandboxGateway, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("sandbox is required")
	}
	if strings.TrimSpace(sourceRepo) == "" {
		return nil, fmt.Errorf("sandbox source repo is required for attestation")
	}

	client, err := NewClient("https://"+sandbox.Domain, sourceRepo, b.execTimeout)
	if err != nil {
		return nil, fmt.Errorf("attest sandbox %s: %w", sandbox.Domain, err)
	}

	token, err := client.Claim(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sandbox %s: %w", sandbox.Domain, err)
	}

	return &SandboxGateway{
		client:            client,
		runtimeCredential: token,
	}, nil
}

func (g *SandboxGateway) Execute(ctx context.Context, callID, rawArgs string, sandboxID string) (Result, error) {
	if g == nil || g.client == nil {
		return Result{}, fmt.Errorf("sandbox gateway is not initialized")
	}
	return g.client.ExecuteDefault(ctx, callID, rawArgs, sandboxID, g.runtimeCredential)
}
