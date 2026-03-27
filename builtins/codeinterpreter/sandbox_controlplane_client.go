package codeinterpreter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const sandboxWorkloadCodeInterpreter = "toolexec.code-interpreter"

// SandboxInfo holds the runtime details of a provisioned sandbox.
type SandboxInfo struct {
	ID        string
	Domain    string
	ExpiresAt time.Time
}

// SandboxSpec describes the workload and parameters for creating a sandbox.
type SandboxSpec struct {
	Workload   string
	SourceRepo string
	TTLSeconds int32
}

type sandboxCreateRequest struct {
	Workload string `json:"workload,omitempty"`
	TTL      int32  `json:"ttl,omitempty"`
}

type sandboxCreateResponse struct {
	SandboxID string `json:"sandbox_id"`
	Domain    string `json:"domain"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

type sandboxGetResponse struct {
	SandboxID string `json:"sandbox_id"`
	Domain    string `json:"domain"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
	LastError string `json:"last_error"`
}

type SandboxControlplaneClient struct {
	baseURL      string
	httpClient   *http.Client
	pollInterval time.Duration
	callerAPIKey string
}

func NewSandboxControlplaneClient(baseURL, callerAPIKey string) *SandboxControlplaneClient {
	return &SandboxControlplaneClient{
		baseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		pollInterval: 2 * time.Second,
		callerAPIKey: strings.TrimSpace(callerAPIKey),
	}
}

// CreateSandbox creates a sandbox and polls until it is ready or the context is cancelled.
func (c *SandboxControlplaneClient) CreateSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("sandbox controlplane client is not configured")
	}

	payload, err := json.Marshal(sandboxCreateRequest{
		Workload: spec.Workload,
		TTL:      spec.TTLSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal sandbox create request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/sandboxes/", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build sandbox create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.callerAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.callerAPIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create sandbox request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read create sandbox response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("create sandbox returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed sandboxCreateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode sandbox create response: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, parsed.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse sandbox expiry: %w", err)
	}

	sandboxID := strings.TrimSpace(parsed.SandboxID)
	ready, err := c.pollUntilReady(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if ready == nil {
		return nil, fmt.Errorf("sandbox deployment completed without status details")
	}
	if strings.TrimSpace(ready.Domain) != "" {
		parsed.Domain = ready.Domain
	}
	if strings.TrimSpace(ready.ExpiresAt) != "" {
		parsed.ExpiresAt = ready.ExpiresAt
	}
	expiresAt, err = time.Parse(time.RFC3339Nano, parsed.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse ready sandbox expiry: %w", err)
	}

	return &SandboxInfo{
		ID:        sandboxID,
		Domain:    strings.TrimSpace(parsed.Domain),
		ExpiresAt: expiresAt,
	}, nil
}

// GetSandbox fetches the current state of a sandbox by ID.
func (c *SandboxControlplaneClient) GetSandbox(ctx context.Context, sandboxID string) (*SandboxInfo, error) {
	s, err := c.getSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, s.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse sandbox expiry: %w", err)
	}
	return &SandboxInfo{
		ID:        strings.TrimSpace(s.SandboxID),
		Domain:    strings.TrimSpace(s.Domain),
		ExpiresAt: expiresAt,
	}, nil
}

// pollUntilReady polls GET /api/sandboxes/:id until the sandbox reaches ready or failed status.
func (c *SandboxControlplaneClient) pollUntilReady(ctx context.Context, sandboxID string) (*sandboxGetResponse, error) {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			s, err := c.getSandbox(ctx, sandboxID)
			if err != nil {
				return nil, fmt.Errorf("poll sandbox status: %w", err)
			}
			switch s.Status {
			case "ready":
				return s, nil
			case "failed":
				if s.LastError != "" {
					return nil, fmt.Errorf("sandbox deployment failed: %s", s.LastError)
				}
				return nil, fmt.Errorf("sandbox deployment failed")
			}
		}
	}
}

func (c *SandboxControlplaneClient) getSandbox(ctx context.Context, sandboxID string) (*sandboxGetResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/sandboxes/"+sandboxID, nil)
	if err != nil {
		return nil, fmt.Errorf("build sandbox get request: %w", err)
	}
	if c.callerAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.callerAPIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get sandbox request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read sandbox response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("get sandbox returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed sandboxGetResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode sandbox get response: %w", err)
	}
	return &parsed, nil
}

func (c *SandboxControlplaneClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	if c == nil || c.baseURL == "" || strings.TrimSpace(sandboxID) == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/sandboxes/"+strings.TrimSpace(sandboxID), nil)
	if err != nil {
		return fmt.Errorf("build sandbox delete request: %w", err)
	}
	if c.callerAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.callerAPIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete sandbox request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		payload, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete sandbox returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	return nil
}
