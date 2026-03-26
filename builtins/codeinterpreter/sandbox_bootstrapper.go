package codeinterpreter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	runtimeCredential, err := generateRuntimeCredential()
	if err != nil {
		return nil, fmt.Errorf("generate sandbox runtime credential: %w", err)
	}

	// Attestation is performed when constructing the secure client. The guest-side
	// bootstrap wire protocol is intentionally left out of this change; the router
	// now owns the credential and the attested transport boundary.
	client, err := NewClient("https://"+sandbox.Domain, sourceRepo, b.execTimeout)
	if err != nil {
		return nil, fmt.Errorf("attest sandbox %s: %w", sandbox.Domain, err)
	}

	return &SandboxGateway{
		client:            client,
		runtimeCredential: runtimeCredential,
	}, nil
}

func generateRuntimeCredential() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func (g *SandboxGateway) Execute(ctx context.Context, callID, rawArgs string, sandboxID string) (Result, error) {
	if g == nil || g.client == nil {
		return Result{}, fmt.Errorf("sandbox gateway is not initialized")
	}
	return g.client.ExecuteDefault(ctx, callID, rawArgs, sandboxID, g.runtimeCredential)
}
