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

type SandboxSpec struct {
	Workload   string
	Image      string
	SourceRepo string
}

type Sandbox struct {
	ID        string
	Domain    string
	ExpiresAt time.Time
}

type sandboxCreateRequest struct {
	Image          string `json:"image"`
	SourceRepo     string `json:"source_repo"`
	TTL            int32  `json:"ttl,omitempty"`
	CallerAPIKeyID string `json:"caller_api_key_id,omitempty"`
}

type sandboxCreateResponse struct {
	SandboxID string `json:"sandbox_id"`
	Domain    string `json:"domain"`
	ExpiresAt string `json:"expires_at"`
}

type SandboxControlplaneClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewSandboxControlplaneClient(baseURL, apiKey string) *SandboxControlplaneClient {
	return &SandboxControlplaneClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		httpClient: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}
}

func (c *SandboxControlplaneClient) CreateSandbox(ctx context.Context, spec SandboxSpec, session *Session, callerAPIKeyID string) (*Sandbox, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("sandbox controlplane client is not configured")
	}

	payload, err := json.Marshal(sandboxCreateRequest{
		Image:          spec.Image,
		SourceRepo:     spec.SourceRepo,
		TTL:            session.TTLSeconds,
		CallerAPIKeyID: strings.TrimSpace(callerAPIKeyID),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal sandbox create request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/sandbox", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build sandbox create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create sandbox request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
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

	return &Sandbox{
		ID:        strings.TrimSpace(parsed.SandboxID),
		Domain:    strings.TrimSpace(parsed.Domain),
		ExpiresAt: expiresAt,
	}, nil
}

func (c *SandboxControlplaneClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	if c == nil || c.baseURL == "" || strings.TrimSpace(sandboxID) == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/sandbox/"+strings.TrimSpace(sandboxID), nil)
	if err != nil {
		return fmt.Errorf("build sandbox delete request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
