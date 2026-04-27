// Quick debug script to figure out what the enclave accepts.
// Run: TINFOIL_API_KEY=... go run debug.go
//
// Tries progressively more complex requests:
//   1. Plain chat completion (no tools)
//   2. Chat with tools declared but no tool_calls in history
//   3. Chat with assistant tool_call + tool result in history
//   4. Same as 3 but building the request as raw JSON via curl-style
//   5. Tool call history, no tools array

//go:build ignore

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	tinfoil "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

const (
	defaultEnclave = "gpt-oss-120b-0.inf6.tinfoil.sh"
	defaultRepo    = "tinfoilsh/confidential-gpt-oss-120b"
	defaultModel   = "gpt-oss-120b"
)

func main() {
	apiKey := os.Getenv("TINFOIL_API_KEY")
	if apiKey == "" {
		log.Fatal("set TINFOIL_API_KEY")
	}

	enclave := defaultEnclave
	if v := os.Getenv("ENCLAVE"); v != "" {
		enclave = v
	}
	repo := defaultRepo
	if v := os.Getenv("REPO"); v != "" {
		repo = v
	}
	model := defaultModel
	if v := os.Getenv("MODEL"); v != "" {
		model = v
	}

	fmt.Printf("Enclave: %s\nRepo:    %s\nModel:   %s\n\n", enclave, repo, model)

	// Verify the enclave and get a TLS-pinned HTTP client.
	fmt.Println("Verifying enclave attestation...")
	sc := tinfoil.NewSecureClient(enclave, repo)
	httpClient, err := sc.HTTPClient()
	if err != nil {
		log.Fatalf("attestation failed: %v", err)
	}
	httpClient.Timeout = 3 * time.Minute
	fmt.Printf("OK — TLS key: %s\n\n", sc.GroundTruth().TLSPublicKey[:16]+"...")

	baseURL := "https://" + enclave + "/v1"

	sdkClient := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
	)
	ctx := context.Background()

	// ── Test 1: Plain completion ──
	fmt.Println("═══ Test 1: Plain chat completion (no tools) ═══")
	{
		resp, err := sdkClient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: openai.ChatModel(model),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("Say hello in one sentence."),
			},
			MaxTokens: openai.Int(64),
		})
		if err != nil {
			fmt.Printf("FAIL: %v\n\n", err)
		} else {
			fmt.Printf("OK: %s\n\n", resp.Choices[0].Message.Content)
		}
	}

	// ── Test 2: Tools declared, no tool_calls in history ──
	fmt.Println("═══ Test 2: Tools declared, no tool history ═══")
	{
		resp, err := sdkClient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: openai.ChatModel(model),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("Say hello in one sentence."),
			},
			Tools:     []openai.ChatCompletionToolUnionParam{searchToolDef()},
			MaxTokens: openai.Int(64),
		})
		if err != nil {
			fmt.Printf("FAIL: %v\n\n", err)
		} else {
			fmt.Printf("OK: %s\n\n", resp.Choices[0].Message.Content)
		}
	}

	// ── Test 3: Full tool call history via SDK ──
	fmt.Println("═══ Test 3: Tool call history via SDK ═══")
	{
		resp, err := sdkClient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: openai.ChatModel(model),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("What is 2+2?"),
				{OfAssistant: &openai.ChatCompletionAssistantMessageParam{
					ToolCalls: []openai.ChatCompletionMessageToolCallUnionParam{
						{OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: "call_1",
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      "search",
								Arguments: `{"query":"2+2"}`,
							},
						}},
					},
				}},
				openai.ToolMessage("The answer is 4.", "call_1"),
			},
			Tools:     []openai.ChatCompletionToolUnionParam{searchToolDef()},
			MaxTokens: openai.Int(64),
		})
		if err != nil {
			fmt.Printf("FAIL: %v\n\n", err)
		} else {
			fmt.Printf("OK: %s\n\n", resp.Choices[0].Message.Content)
		}
	}

	// ── Test 4: Same thing as raw JSON POST ──
	fmt.Println("═══ Test 4: Tool call history via raw JSON ═══")
	{
		body := map[string]any{
			"model": model,
			"messages": []map[string]any{
				{"role": "user", "content": "What is 2+2?"},
				{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
					{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "search",
							"arguments": `{"query":"2+2"}`,
						},
					},
				}},
				{"role": "tool", "tool_call_id": "call_1", "content": "The answer is 4."},
			},
			"tools": []map[string]any{
				{
					"type": "function",
					"function": map[string]any{
						"name":        "search",
						"description": "Search the web.",
						"parameters": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"query": map[string]any{"type": "string"},
							},
							"required": []string{"query"},
						},
					},
				},
			},
			"max_tokens": 64,
		}

		content, status, rawResp, err := rawPost(httpClient, baseURL+"/chat/completions", apiKey, body)
		if err != nil {
			fmt.Printf("FAIL (network): %v\n\n", err)
		} else if status != 200 {
			fmt.Printf("FAIL (HTTP %d):\n%s\n\n", status, rawResp)
		} else {
			fmt.Printf("OK: %s\n\n", content)
		}
	}

	// ── Test 5: Raw JSON without tools array ──
	fmt.Println("═══ Test 5: Tool call history, no tools array ═══")
	{
		body := map[string]any{
			"model": model,
			"messages": []map[string]any{
				{"role": "user", "content": "What is 2+2?"},
				{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
					{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "search",
							"arguments": `{"query":"2+2"}`,
						},
					},
				}},
				{"role": "tool", "tool_call_id": "call_1", "content": "The answer is 4."},
			},
			"max_tokens": 64,
		}

		content, status, rawResp, err := rawPost(httpClient, baseURL+"/chat/completions", apiKey, body)
		if err != nil {
			fmt.Printf("FAIL (network): %v\n\n", err)
		} else if status != 200 {
			fmt.Printf("FAIL (HTTP %d):\n%s\n\n", status, rawResp)
		} else {
			fmt.Printf("OK: %s\n\n", content)
		}
	}
}

func searchToolDef() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionToolUnionParam{
		OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        "search",
				Description: openai.String("Search the web."),
				Parameters: shared.FunctionParameters{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []string{"query"},
				},
			},
		},
	}
}

func rawPost(httpClient *http.Client, url, apiKey string, body any) (content string, status int, rawResp string, err error) {
	jsonBody, _ := json.Marshal(body)
	fmt.Printf("Request body:\n%s\n\n", indentJSON(jsonBody))

	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return "", 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	rawResp = string(respBody)

	if resp.StatusCode != 200 {
		return "", resp.StatusCode, rawResp, nil
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", resp.StatusCode, rawResp, fmt.Errorf("parse response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", resp.StatusCode, rawResp, fmt.Errorf("no choices")
	}
	return result.Choices[0].Message.Content, resp.StatusCode, rawResp, nil
}

func indentJSON(b []byte) string {
	var buf bytes.Buffer
	json.Indent(&buf, b, "", "  ")
	return buf.String()
}
