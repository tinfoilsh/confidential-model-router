package codeinterpreter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

// Output is a single output item from a code execution.
type Output struct {
	Type string `json:"type"`
	Logs string `json:"logs,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Client is a simple HTTP client for the code-interpreter service.
// It has no knowledge of sessions or sandbox lifecycle.
type Client struct {
	baseURL     string
	httpClient  *http.Client
	execTimeout time.Duration
}

// NewClient constructs an attested code interpreter client for baseURL.
// It performs full attestation via tinfoil-go before the first request.
func NewClient(baseURL, repo string, execTimeout time.Duration) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, fmt.Errorf("code interpreter base URL is required")
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, fmt.Errorf("code interpreter source repo is required for attestation")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid code interpreter base URL %q: %w", trimmed, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("code interpreter base URL must use HTTPS for attestation (got scheme %q)", u.Scheme)
	}
	if execTimeout <= 0 {
		execTimeout = 30 * time.Second
	}

	sc := tinfoilClient.NewSecureClient(u.Host, repo)
	httpClient, err := sc.HTTPClient()
	if err != nil {
		return nil, fmt.Errorf("attest code interpreter enclave %s: %w", u.Host, err)
	}
	httpClient.Timeout = 2 * time.Minute

	return &Client{
		baseURL:     trimmed,
		httpClient:  httpClient,
		execTimeout: execTimeout,
	}, nil
}

// Claim binds this client to the sandbox and returns the bearer token
// required for all subsequent requests. Call exactly once after attestation.
func (c *Client) Claim(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/claim", nil)
	if err != nil {
		return "", fmt.Errorf("build claim request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("claim request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("claim returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decode claim response: %w", err)
	}
	if strings.TrimSpace(parsed.Token) == "" {
		return "", fmt.Errorf("claim returned empty token")
	}
	return parsed.Token, nil
}

// CreateContext creates a new Python kernel context and returns its ID.
func (c *Client) CreateContext(ctx context.Context, token string) (string, error) {
	body, err := json.Marshal(createContextRequest{Language: "python"})
	if err != nil {
		return "", fmt.Errorf("marshal create context request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/contexts", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build create context request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create context request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		payload, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create context returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	var created createContextResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("decode create context response: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", fmt.Errorf("create context returned empty id")
	}
	return strings.TrimSpace(created.ID), nil
}

// Execute runs code in the given context and returns outputs, exit code, and any transport error.
func (c *Client) Execute(ctx context.Context, contextID string, args Args, token string) ([]Output, int, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if c.execTimeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.execTimeout)
		defer cancel()
	}

	body, err := json.Marshal(executionRequest{
		Code:      args.Code,
		ContextID: contextID,
	})
	if err != nil {
		return nil, -1, fmt.Errorf("marshal execute request: %w", err)
	}

	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, c.baseURL+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, -1, fmt.Errorf("build execute request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, -1, fmt.Errorf("execute request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		payload, _ := io.ReadAll(resp.Body)
		return nil, -1, fmt.Errorf("execute returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	exitCode := 0
	var stdout strings.Builder
	var stderr strings.Builder
	var mediaOutputs []Output

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var item executeStreamItem
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, -1, fmt.Errorf("decode execute stream item: %w", err)
		}
		switch item.Type {
		case "stdout":
			stdout.WriteString(item.Text)
		case "stderr":
			stderr.WriteString(item.Text)
		case "result":
			if strings.TrimSpace(item.Text) != "" {
				stdout.WriteString(item.Text)
				if !strings.HasSuffix(item.Text, "\n") {
					stdout.WriteString("\n")
				}
			}
			if item.Png != "" {
				mediaOutputs = append(mediaOutputs, Output{Type: "image", URL: "data:image/png;base64," + item.Png})
			}
			if item.Jpeg != "" {
				mediaOutputs = append(mediaOutputs, Output{Type: "image", URL: "data:image/jpeg;base64," + item.Jpeg})
			}
			if item.Svg != "" {
				mediaOutputs = append(mediaOutputs, Output{Type: "image", URL: "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(item.Svg))})
			}
			if item.Pdf != "" {
				mediaOutputs = append(mediaOutputs, Output{Type: "image", URL: "data:application/pdf;base64," + item.Pdf})
			}
		case "error":
			exitCode = 1
			if item.Name != "" {
				stderr.WriteString(item.Name)
			}
			if item.Value != "" {
				if stderr.Len() > 0 {
					stderr.WriteString(": ")
				}
				stderr.WriteString(item.Value)
			}
			if item.Traceback != "" {
				if stderr.Len() > 0 && !strings.HasSuffix(stderr.String(), "\n") {
					stderr.WriteString("\n")
				}
				stderr.WriteString(item.Traceback)
			}
			if stderr.Len() > 0 && !strings.HasSuffix(stderr.String(), "\n") {
				stderr.WriteString("\n")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, -1, fmt.Errorf("read execute stream: %w", err)
	}

	logs := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	logs = strings.TrimSpace(logs)
	var outputs []Output
	if logs != "" {
		outputs = append(outputs, Output{Type: "logs", Logs: logs})
	}
	outputs = append(outputs, mediaOutputs...)
	return outputs, exitCode, nil
}

// DeleteContext deletes a kernel context by ID.
func (c *Client) DeleteContext(ctx context.Context, contextID, token string) error {
	contextID = strings.TrimSpace(contextID)
	if contextID == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/contexts/"+url.PathEscape(contextID), nil)
	if err != nil {
		return fmt.Errorf("build delete context request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete context request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		payload, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete context returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return nil
}

// Private types for HTTP request/response encoding.

type claimResponse struct {
	Token string `json:"token"`
}

type executionRequest struct {
	Code      string `json:"code"`
	ContextID string `json:"context_id,omitempty"`
}

type createContextRequest struct {
	Language string `json:"language"`
}

type createContextResponse struct {
	ID string `json:"id"`
}

type executeStreamItem struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Png       string `json:"png"`
	Jpeg      string `json:"jpeg"`
	Svg       string `json:"svg"`
	Pdf       string `json:"pdf"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Traceback string `json:"traceback"`
}
