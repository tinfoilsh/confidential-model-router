// Package e2e contains end-to-end tests for the confidential-model-router.
//
// Configuration is loaded from a .env file in this directory (if present),
// then from environment variables. Run against a live router:
//
//	go test ./... -v -timeout 30m
//
// Environment variables (all optional — .env provides defaults):
//
//	E2E_TINFOIL_ENCLAVE         enclave hostname for attested mode (e.g. "inference.tinfoil.sh")
//	E2E_TINFOIL_REPO            GitHub repo for attestation (default: "tinfoilsh/confidential-model-router")
//	E2E_ROUTER_URL              plain router URL for local/insecure mode (default: http://localhost:8089)
//	E2E_API_KEY                 bearer token for all requests
//	E2E_MODEL                   default chat model (default: gpt-oss-120b)
//	E2E_RESPONSES_MODEL         model for /v1/responses (defaults to E2E_MODEL)
//	E2E_CI_AVAILABLE            set to "true" to run code-interpreter tests
//	E2E_WEBSEARCH_AVAILABLE     set to "true" to run websearch tests
//	E2E_RESPONSES_API_AVAILABLE set to "true" to run /v1/responses tests
//
// When E2E_TINFOIL_ENCLAVE is set the client performs full enclave attestation.
// Otherwise it falls back to an insecure plain HTTP client against E2E_ROUTER_URL,
// which is suitable for local development where the router runs without a TEE.
package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	tinfoil "github.com/tinfoilsh/tinfoil-go"
)

// TestMain loads .env (if present) before running any test, mirroring the
// pattern used in tc-shell live integration tests.
func TestMain(m *testing.M) {
	loadDotEnv(".env")
	os.Exit(m.Run())
}

// loadDotEnv reads KEY=VALUE pairs from path and sets them as environment
// variables, skipping lines that are blank or start with '#'.
// Variables already set in the environment take precedence (no overwrite).
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // .env is optional
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" {
			continue
		}
		// Don't overwrite variables already set by the caller.
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// ─── Configuration ────────────────────────────────────────────────────────────

type testCfg struct {
	TinfoilEnclave        string // empty → insecure mode against RouterURL
	TinfoilRepo           string
	RouterURL             string // used only in insecure mode
	APIKey                string
	Model                 string
	ResponsesModel        string // model that supports /v1/responses
	CIAvailable           bool
	WebsearchAvailable    bool
	ResponsesAPIAvailable bool
}

func getConfig(t *testing.T) testCfg {
	t.Helper()
	model := envOrDefault("E2E_MODEL", "gpt-oss-120b")
	return testCfg{
		TinfoilEnclave:        os.Getenv("E2E_TINFOIL_ENCLAVE"),
		TinfoilRepo:           envOrDefault("E2E_TINFOIL_REPO", "tinfoilsh/confidential-model-router"),
		RouterURL:             envOrDefault("E2E_ROUTER_URL", "http://localhost:8089"),
		APIKey:                os.Getenv("E2E_API_KEY"),
		Model:                 model,
		ResponsesModel:        envOrDefault("E2E_RESPONSES_MODEL", model),
		CIAvailable:           os.Getenv("E2E_CI_AVAILABLE") == "true",
		WebsearchAvailable:    os.Getenv("E2E_WEBSEARCH_AVAILABLE") == "true",
		ResponsesAPIAvailable: os.Getenv("E2E_RESPONSES_API_AVAILABLE") == "true",
	}
}

func (cfg testCfg) requireCI(t *testing.T) {
	t.Helper()
	if !cfg.CIAvailable {
		t.Skip("set E2E_CI_AVAILABLE=true to run code-interpreter tests")
	}
}

func (cfg testCfg) requireWebsearch(t *testing.T) {
	t.Helper()
	if !cfg.WebsearchAvailable {
		t.Skip("set E2E_WEBSEARCH_AVAILABLE=true to run websearch tests")
	}
}

func (cfg testCfg) requireResponsesAPI(t *testing.T) {
	t.Helper()
	if !cfg.ResponsesAPIAvailable {
		t.Skip("set E2E_RESPONSES_API_AVAILABLE=true to run /v1/responses tests (requires a model that supports the Responses API)")
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ─── Tinfoil / OpenAI SDK client ─────────────────────────────────────────────

// newTinfoilClient returns an OpenAI client and its underlying HTTP client.
//
// When E2E_TINFOIL_ENCLAVE is set it performs full enclave attestation.
// Otherwise it returns an unverified EHBP client against E2E_ROUTER_URL, which
// is suitable for local development without a TEE.
func newTinfoilClient(t *testing.T, cfg testCfg) (openai.Client, *http.Client) {
	t.Helper()
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = "placeholder"
	}
	if cfg.TinfoilEnclave != "" {
		tfClient, err := tinfoil.NewClientWithParams(cfg.TinfoilEnclave, cfg.TinfoilRepo,
			option.WithAPIKey(apiKey),
			option.WithMaxRetries(0),
		)
		if err != nil {
			t.Fatalf("tinfoil attestation failed: %v", err)
		}
		return *tfClient.Client, tfClient.HTTPClient()
	}
	uClient, err := tinfoil.NewUnverifiedClient(tinfoil.UnverifiedClientOptions{BaseURL: cfg.RouterURL + "/v1/"},
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("failed to create unverified client: %v", err)
	}
	return *uClient.Client, uClient.HTTPClient()
}

// newOpenAIClient returns an OpenAI-compatible client for the router under test.
func newOpenAIClient(t *testing.T, cfg testCfg) openai.Client {
	t.Helper()
	c, _ := newTinfoilClient(t, cfg)
	return c
}

// newResponsesClient returns the Responses service from the router client.
func newResponsesClient(t *testing.T, cfg testCfg) responses.ResponseService {
	t.Helper()
	return newOpenAIClient(t, cfg).Responses
}

// background returns a context.Background() for use in test calls.
func background() context.Context {
	return context.Background()
}

// ─── Raw HTTP client helpers ───────────────────────────────────────────────────
// Used for endpoints without SDK support (health check, embeddings, raw error tests).

type routerClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newRouterClient(t *testing.T, cfg testCfg) *routerClient {
	t.Helper()
	_, httpClient := newTinfoilClient(t, cfg)
	baseURL := cfg.RouterURL
	if cfg.TinfoilEnclave != "" {
		baseURL = "https://" + cfg.TinfoilEnclave
	}
	return &routerClient{
		baseURL: baseURL,
		apiKey:  cfg.APIKey,
		http:    httpClient,
	}
}

func (c *routerClient) postJSON(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("do request %s: %v", path, err)
	}
	return resp
}

func (c *routerClient) getJSON(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("do request %s: %v", path, err)
	}
	return resp
}

// ─── Raw HTTP response helpers ─────────────────────────────────────────────────

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

func requireStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("want status %d, got %d: %s", want, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func requireNoError(t *testing.T, body map[string]any) {
	t.Helper()
	if errField, ok := body["error"]; ok && errField != nil {
		raw, _ := json.Marshal(errField)
		t.Fatalf("unexpected API error: %s", string(raw))
	}
}

func requireErrorType(t *testing.T, body map[string]any, wantType string) {
	t.Helper()
	errField, ok := body["error"].(map[string]any)
	if !ok || errField == nil {
		t.Fatal("expected error in response but found none")
	}
	gotType, _ := errField["type"].(string)
	if gotType != wantType {
		t.Errorf("error type: want %q, got %q (message: %s)", wantType, gotType, errField["message"])
	}
}

// ─── SDK error helpers ────────────────────────────────────────────────────────

// requireAPIError checks that an error is an *openai.Error with the expected status code.
// Returns the raw JSON body for further inspection.
func requireAPIError(t *testing.T, err error, wantStatus int) map[string]any {
	t.Helper()
	if err == nil {
		t.Fatalf("expected API error with status %d, got nil", wantStatus)
	}
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *openai.Error, got: %v", err)
	}
	if apiErr.StatusCode != wantStatus {
		t.Fatalf("want status %d, got %d: %s", wantStatus, apiErr.StatusCode, apiErr.RawJSON())
	}
	// The SDK unwraps the "error" key from the response body before storing
	// in RawJSON(), so re-wrap it to match the raw HTTP response format that
	// requireErrorType and other helpers expect.
	var inner map[string]any
	_ = json.Unmarshal([]byte(apiErr.RawJSON()), &inner)
	return map[string]any{"error": inner}
}

// requireAPIErrorType checks that an error is an openai.Error with the expected status
// code AND that body["error"]["type"] matches wantType.
func requireAPIErrorType(t *testing.T, err error, wantStatus int, wantType string) {
	t.Helper()
	body := requireAPIError(t, err, wantStatus)
	requireErrorType(t, body, wantType)
}

// ─── Chat helpers ─────────────────────────────────────────────────────────────

// ciToolChoiceParam builds the tool_choice param for forcing code_interpreter calls.
func ciToolChoiceParam() openai.ChatCompletionToolChoiceOptionUnionParam {
	return openai.ChatCompletionToolChoiceOptionUnionParam{
		OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
			Function: openai.ChatCompletionNamedToolChoiceFunctionParam{
				Name: "code_interpreter",
			},
		},
	}
}

// ciToolChoiceForResponses builds the tool_choice param for responses API CI calls.
func ciToolChoiceForResponses() responses.ResponseNewParamsToolChoiceUnion {
	return responses.ResponseNewParamsToolChoiceUnion{
		OfHostedTool: &responses.ToolChoiceTypesParam{
			Type: "code_interpreter",
		},
	}
}

// codeInterpreterTool returns a responses ToolUnionParam for the code_interpreter built-in.
func codeInterpreterTool() responses.ToolUnionParam {
	return responses.ToolUnionParam{
		OfCodeInterpreter: &responses.ToolCodeInterpreterParam{
			Container: responses.ToolCodeInterpreterContainerUnionParam{
				OfCodeInterpreterToolAuto: &responses.ToolCodeInterpreterContainerCodeInterpreterContainerAutoParam{},
			},
		},
	}
}

// webSearchTool returns a responses ToolUnionParam for the web_search built-in.
func webSearchTool() responses.ToolUnionParam {
	return responses.ToolUnionParam{
		OfWebSearch: &responses.WebSearchToolParam{Type: "web_search"},
	}
}

// responseInput returns a ResponseNewParamsInputUnion for a plain string input.
func responseInput(s string) responses.ResponseNewParamsInputUnion {
	return responses.ResponseNewParamsInputUnion{
		OfString: openai.Opt(s),
	}
}

// ─── CI output helpers ────────────────────────────────────────────────────────

// ciOutputItemsFromResponse returns code_interpreter_call items from a Response.
func ciOutputItemsFromResponse(resp *responses.Response) []responses.ResponseOutputItemUnion {
	var result []responses.ResponseOutputItemUnion
	for _, item := range resp.Output {
		if item.Type == "code_interpreter_call" {
			result = append(result, item)
		}
	}
	return result
}

// requireCIItemExecuted verifies a code_interpreter_call output item has status and container_id.
func requireCIItemExecuted(t *testing.T, item responses.ResponseOutputItemUnion) {
	t.Helper()
	if item.Status == "" {
		t.Error("code_interpreter_call item missing status field")
	}
	if item.ContainerID == "" {
		t.Error("code_interpreter_call item missing container_id field")
	}
	t.Logf("code_interpreter_call: status=%s container_id=%s code=%q",
		item.Status, item.ContainerID, item.Code)
}

// ─── Chat CI raw JSON helpers ─────────────────────────────────────────────────
// The router extends tool_calls with non-standard fields (status, container_id, outputs).
// These helpers extract those custom fields from the raw JSON of a chat completion.

type rawCIToolCall struct {
	ID          string        `json:"id"`
	Status      string        `json:"status"`
	ContainerID string        `json:"container_id"`
	Outputs     []rawCIOutput `json:"outputs"`
	Function    struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type rawCIOutput struct {
	Type string `json:"type"`
	Logs string `json:"logs"`
	URL  string `json:"url"`
}

type rawChatCompletion struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			Reasoning string         `json:"reasoning"`
			ToolCalls []rawCIToolCall `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			Reasoning        string         `json:"reasoning"`
			ToolCalls        []rawCIToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// parseChatCompletion unmarshals a raw JSON string into rawChatCompletion.
func parseChatCompletion(t *testing.T, rawJSON string) rawChatCompletion {
	t.Helper()
	var raw rawChatCompletion
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("unmarshal chat completion: %v", err)
	}
	return raw
}

// ciToolCallsFromRaw returns code_interpreter tool_calls from a rawChatCompletion message.
func ciToolCallsFromRaw(raw rawChatCompletion) []rawCIToolCall {
	if len(raw.Choices) == 0 {
		return nil
	}
	var result []rawCIToolCall
	for _, tc := range raw.Choices[0].Message.ToolCalls {
		if tc.Function.Name == "code_interpreter" {
			result = append(result, tc)
		}
	}
	return result
}

// requireCIToolCallExecuted verifies a raw code_interpreter tool_call has expected fields.
func requireCIToolCallExecuted(t *testing.T, tc rawCIToolCall) {
	t.Helper()
	if tc.Status == "" {
		t.Error("code_interpreter tool_call missing status field")
	}
	if tc.ContainerID == "" {
		t.Error("code_interpreter tool_call missing container_id field")
	}
	t.Logf("code_interpreter: status=%s container_id=%s outputs=%d",
		tc.Status, tc.ContainerID, len(tc.Outputs))
}

// logJSON logs a value as indented JSON.
func logJSON(t *testing.T, label string, v any) {
	t.Helper()
	raw, _ := json.MarshalIndent(v, "", "  ")
	t.Logf("%s:\n%s", label, string(raw))
}
