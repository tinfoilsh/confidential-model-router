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

const (
	StatusInProgress   = "in_progress"
	StatusInterpreting = "interpreting"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
)

type AutoConfig struct {
	TTLSeconds int32
}

type ContainerConfig struct {
	ContainerID string
	Auto        *AutoConfig
}

type Session struct {
	ContainerID string
	Managed     bool
	TTLSeconds  int32
}

type Args struct {
	Code        string `json:"code"`
	ContainerID string `json:"container_id,omitempty"`
}

type Output struct {
	Type string `json:"type"`
	Logs string `json:"logs,omitempty"`
	URL  string `json:"url,omitempty"`
}

type Result struct {
	ID          string   `json:"id,omitempty"`
	Code        string   `json:"code,omitempty"`
	ContainerID string   `json:"container_id,omitempty"`
	Status      string   `json:"status"`
	Outputs     []Output `json:"outputs,omitempty"`
	ExitCode    int      `json:"exit_code"`
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

type Client struct {
	baseURL     string
	httpClient  *http.Client
	execTimeout time.Duration
}

// NewClient constructs a code interpreter client for baseURL.
// When repo is non-empty (e.g. "tinfoilsh/code-interpreter") the client
// performs full attestation via tinfoil-go's SecureClient before the first
// request, mirroring the trust chain used for inference model enclaves.
// When repo is empty the client uses a plain http.Client (local dev / debug).
func NewClient(baseURL, repo string, execTimeout time.Duration) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, fmt.Errorf("code interpreter base URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid code interpreter base URL %q: %w", trimmed, err)
	}
	if repo != "" && u.Scheme != "https" {
		return nil, fmt.Errorf("code interpreter base URL must use HTTPS for attested clients (got scheme %q)", u.Scheme)
	}
	if execTimeout <= 0 {
		execTimeout = 30 * time.Second
	}

	var httpClient *http.Client
	if repo != "" {
		sc := tinfoilClient.NewSecureClient(u.Host, repo)
		httpClient, err = sc.HTTPClient()
		if err != nil {
			return nil, fmt.Errorf("attest code interpreter enclave %s: %w", u.Host, err)
		}
		httpClient.Timeout = 2 * time.Minute
	} else {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}

	return &Client{
		baseURL:     trimmed,
		httpClient:  httpClient,
		execTimeout: execTimeout,
	}, nil
}

type claimResponse struct {
	Token string `json:"token"`
}

// Claim calls POST /claim on the sandbox, binding it to this client and
// returning the bearer token required for all subsequent requests.
// It must be called exactly once, immediately after attestation.
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

func ParseArgs(raw string) (Args, error) {
	var args Args
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return Args{}, fmt.Errorf("parse code_interpreter arguments: %w", err)
	}
	args.Code = strings.TrimSpace(args.Code)
	args.ContainerID = strings.TrimSpace(args.ContainerID)
	if args.Code == "" {
		return Args{}, fmt.Errorf("parse code_interpreter arguments: code is required")
	}
	return args, nil
}

func NewSession(config ContainerConfig) (*Session, error) {
	if config.Auto == nil && strings.TrimSpace(config.ContainerID) == "" {
		return nil, fmt.Errorf("code interpreter container config is required")
	}
	if config.Auto != nil {
		return &Session{
			Managed:    true,
			TTLSeconds: config.Auto.TTLSeconds,
		}, nil
	}
	return &Session{
		ContainerID: strings.TrimSpace(config.ContainerID),
	}, nil
}

func (c *Client) Execute(ctx context.Context, callID string, rawArgs string, session *Session, apiKey string) (Result, error) {
	args, err := ParseArgs(rawArgs)
	if err != nil {
		return failedResult(callID, session, "", err), nil
	}
	return c.ExecuteArgs(ctx, callID, args, session, apiKey)
}

func (c *Client) ExecuteArgs(ctx context.Context, callID string, args Args, session *Session, apiKey string) (Result, error) {
	result := Result{
		ID:       callID,
		Code:     args.Code,
		Status:   StatusFailed,
		ExitCode: -1,
	}
	if session == nil {
		return failedResult(callID, nil, args.Code, fmt.Errorf("code interpreter session is required")), nil
	}

	containerID, err := c.resolveContainerID(ctx, session, args.ContainerID, apiKey)
	if err != nil {
		return failedResult(callID, session, args.Code, err), nil
	}
	result.ContainerID = containerID

	execution, err := c.execute(ctx, containerID, args.Code, apiKey)
	if err != nil {
		return failedResult(callID, session, args.Code, err), nil
	}

	result.ContainerID = execution.ContainerID
	result.Outputs = execution.Outputs
	result.ExitCode = execution.ExitCode
	if execution.ExitCode == 0 {
		result.Status = StatusCompleted
	} else {
		result.Status = StatusFailed
	}
	return result, nil
}

func (c *Client) ExecuteDefault(ctx context.Context, callID string, rawArgs string, containerID string, apiKey string) (Result, error) {
	args, err := ParseArgs(rawArgs)
	if err != nil {
		return failedResult(callID, nil, "", err), nil
	}

	result := Result{
		ID:          callID,
		Code:        args.Code,
		ContainerID: strings.TrimSpace(containerID),
		Status:      StatusFailed,
		ExitCode:    -1,
	}

	execution, err := c.execute(ctx, "", args.Code, apiKey)
	if err != nil {
		return failedResult(callID, &Session{ContainerID: result.ContainerID}, args.Code, err), nil
	}

	result.Outputs = execution.Outputs
	result.ExitCode = execution.ExitCode
	if execution.ExitCode == 0 {
		result.Status = StatusCompleted
	} else {
		result.Status = StatusFailed
	}
	return result, nil
}

type executeResult struct {
	ContainerID string
	Outputs     []Output
	ExitCode    int
}

func (c *Client) resolveContainerID(ctx context.Context, session *Session, hintedID string, apiKey string) (string, error) {
	hintedID = strings.TrimSpace(hintedID)
	// The session owner (user-supplied code_interpreter_options) has already
	// determined which container to use when ContainerID is set or Managed is
	// true. In both cases the model's hinted container_id is silently ignored:
	// a managed session owns its lifecycle, and a named session was explicitly
	// chosen by the caller. We only apply the hint when no container has been
	// established yet for a non-managed session (rare, forward-compatible path).
	if hintedID != "" && !session.Managed && session.ContainerID == "" {
		session.ContainerID = hintedID
	}

	if strings.TrimSpace(session.ContainerID) != "" {
		return session.ContainerID, nil
	}

	body, err := json.Marshal(createContextRequest{Language: "python"})
	if err != nil {
		return "", fmt.Errorf("marshal create context request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/contexts", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build create context request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
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
	session.ContainerID = strings.TrimSpace(created.ID)
	return session.ContainerID, nil
}

func (c *Client) execute(ctx context.Context, containerID string, code string, apiKey string) (*executeResult, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if c.execTimeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.execTimeout)
		defer cancel()
	}

	body, err := json.Marshal(executionRequest{
		Code:      code,
		ContextID: containerID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal execute request: %w", err)
	}

	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, c.baseURL+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build execute request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("execute returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	result := &executeResult{
		ContainerID: containerID,
		ExitCode:    0,
	}
	var stdout strings.Builder
	var stderr strings.Builder

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var item executeStreamItem
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, fmt.Errorf("decode execute stream item: %w", err)
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
				result.Outputs = append(result.Outputs, Output{
					Type: "image",
					URL:  "data:image/png;base64," + item.Png,
				})
			}
			if item.Jpeg != "" {
				result.Outputs = append(result.Outputs, Output{
					Type: "image",
					URL:  "data:image/jpeg;base64," + item.Jpeg,
				})
			}
			if item.Svg != "" {
				result.Outputs = append(result.Outputs, Output{
					Type: "image",
					URL:  "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(item.Svg)),
				})
			}
			if item.Pdf != "" {
				result.Outputs = append(result.Outputs, Output{
					Type: "image",
					URL:  "data:application/pdf;base64," + item.Pdf,
				})
			}
		case "error":
			result.ExitCode = 1
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
		return nil, fmt.Errorf("read execute stream: %w", err)
	}

	logs := strings.TrimSpace(strings.TrimSpace(stdout.String()) + "\n" + strings.TrimSpace(stderr.String()))
	logs = strings.TrimSpace(logs)
	if logs != "" {
		result.Outputs = append([]Output{{Type: "logs", Logs: logs}}, result.Outputs...)
	}
	return result, nil
}

func failedResult(callID string, session *Session, code string, err error) Result {
	containerID := ""
	if session != nil {
		containerID = session.ContainerID
	}
	message := ""
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	outputs := []Output{}
	if message != "" {
		outputs = append(outputs, Output{Type: "logs", Logs: message})
	}
	return Result{
		ID:          callID,
		Code:        code,
		ContainerID: containerID,
		Status:      StatusFailed,
		Outputs:     outputs,
		ExitCode:    -1,
	}
}

func (c *Client) DeleteContext(ctx context.Context, contextID string, apiKey string) error {
	contextID = strings.TrimSpace(contextID)
	if contextID == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/contexts/"+url.PathEscape(contextID), nil)
	if err != nil {
		return fmt.Errorf("build delete context request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
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
