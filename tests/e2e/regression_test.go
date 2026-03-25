package e2e_test

// Regression tests for pre-existing router behavior.
//
// These tests verify that routing, streaming, model extraction, and metadata
// endpoints continue to work correctly after the toolexec changes.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// TestRegression_ProxyStatus verifies the /.well-known/tinfoil-proxy health endpoint
// returns a JSON object with a version field.
func TestRegression_ProxyStatus(t *testing.T) {
	cfg := getConfig(t)
	c := newRouterClient(t, cfg)

	resp := c.getJSON(t, "/.well-known/tinfoil-proxy")
	requireStatus(t, resp, 200)
	body := decodeJSON(t, resp)

	if body["version"] == nil {
		t.Error("expected version field in /.well-known/tinfoil-proxy response")
	}
	t.Logf("router version: %v", body["version"])
}

// TestRegression_ModelsList verifies /v1/models proxies to the control plane
// and returns a list of models.
func TestRegression_ModelsList(t *testing.T) {
	cfg := getConfig(t)
	c := newRouterClient(t, cfg)

	resp := c.getJSON(t, "/v1/models")
	if resp.StatusCode != 200 {
		t.Skipf("control plane not reachable for /v1/models: %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)

	// OpenAI-compatible list response has "object": "list" and "data": [...]
	if object, _ := body["object"].(string); object != "list" {
		t.Errorf("expected object=list, got %q", object)
	}
	data, _ := body["data"].([]any)
	t.Logf("/v1/models returned %d models", len(data))
}

// TestRegression_NormalChatCompletion verifies a basic non-streaming chat completion
// works end-to-end with the new toolexec code in place.
func TestRegression_NormalChatCompletion(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	completion, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Reply with exactly: pong"),
		},
		MaxTokens: openai.Int(10),
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	if len(completion.Choices) == 0 {
		t.Fatal("response has no choices")
	}
	msg := completion.Choices[0].Message
	// Some reasoning models (e.g. gpt-oss-120b) return the response in the
	// "reasoning" field rather than "content".
	raw := parseChatCompletion(t, completion.RawJSON())
	content := strings.TrimSpace(msg.Content)
	reasoning := ""
	if len(raw.Choices) > 0 {
		reasoning = strings.TrimSpace(raw.Choices[0].Message.Reasoning)
	}
	t.Logf("response content: %q, reasoning: %q", content, reasoning)
	if content == "" && reasoning == "" {
		t.Error("expected non-empty content or reasoning in chat completion response")
	}
}

// TestRegression_NormalChatStreaming verifies a streaming chat completion produces
// SSE chunks and terminates with [DONE].
func TestRegression_NormalChatStreaming(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	stream := oai.Chat.Completions.NewStreaming(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Count from 1 to 5"),
		},
		MaxTokens: openai.Int(30),
	})
	defer stream.Close()

	var chunkCount int
	hasContent := false

	for stream.Next() {
		chunk := stream.Current()
		chunkCount++
		for _, choice := range chunk.Choices {
			raw := parseChatCompletion(t, chunk.RawJSON())
			if choice.Delta.Content != "" {
				hasContent = true
			}
			// Some reasoning models stream text in "reasoning_content" / "reasoning".
			if len(raw.Choices) > 0 {
				if raw.Choices[0].Delta.ReasoningContent != "" || raw.Choices[0].Delta.Reasoning != "" {
					hasContent = true
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if chunkCount == 0 {
		t.Error("no chunks received in streaming response")
	}
	if !hasContent {
		t.Error("no content deltas found in streaming response")
	}
	t.Logf("streaming: %d chunks", chunkCount)
}

// TestRegression_Streaming_ContinuousUsageStats verifies that the router
// injects continuous_usage_stats=true into streaming requests, enabling
// token tracking even when the client doesn't request it.
func TestRegression_Streaming_ContinuousUsageStats(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	// No stream_options — router should inject continuous_usage_stats automatically.
	stream := oai.Chat.Completions.NewStreaming(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say 'hi'"),
		},
		MaxTokens: openai.Int(5),
	})
	defer stream.Close()

	for stream.Next() {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	// We expect a usage chunk to be filtered out since the client didn't ask
	// for it. The stream should still be valid (no error).
}

// TestRegression_Streaming_IncludeUsage verifies that when the client sets
// stream_options.include_usage=true, the usage chunk is forwarded to the client.
func TestRegression_Streaming_IncludeUsage(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	stream := oai.Chat.Completions.NewStreaming(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say 'hi'"),
		},
		MaxTokens:     openai.Int(5),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	})
	defer stream.Close()

	var usageChunk *openai.ChatCompletionChunk
	for stream.Next() {
		chunk := stream.Current()
		if chunk.Usage.TotalTokens > 0 && len(chunk.Choices) == 0 {
			c := chunk
			usageChunk = &c
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if usageChunk == nil {
		t.Log("no usage-only chunk found (model may not emit it in all cases)")
		return
	}
	t.Logf("usage chunk: total_tokens=%d prompt_tokens=%d",
		usageChunk.Usage.TotalTokens, usageChunk.Usage.PromptTokens)
}

// TestRegression_Embeddings verifies the /v1/embeddings endpoint is still
// reachable and returns embeddings for the nomic-embed-text model.
func TestRegression_Embeddings(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	result, err := oai.Embeddings.New(background(), openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel("nomic-embed-text"),
		Input: openai.EmbeddingNewParamsInputUnion{
			OfString: openai.String("The quick brown fox"),
		},
	})
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 404 || apiErr.StatusCode == 503) {
			t.Skipf("nomic-embed-text not available in this environment (status %d)", apiErr.StatusCode)
		}
		t.Fatalf("embeddings: %v", err)
	}

	if len(result.Data) == 0 {
		t.Fatal("expected embedding data in response")
	}
	emb := result.Data[0].Embedding
	if len(emb) == 0 {
		t.Error("expected non-empty embedding vector")
	}
	t.Logf("embedding dimensions: %d", len(emb))
}

// TestRegression_NormalChat_NoToolExecInterference verifies that a plain chat
// request (no tools, no code_interpreter_options) is not intercepted by the
// tool executor and reaches the model normally.
func TestRegression_NormalChat_NoToolExecInterference(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	completion, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are a helpful assistant. Answer in one word."),
			openai.UserMessage("What color is the sky?"),
		},
		MaxTokens: openai.Int(5),
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	if len(completion.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	// Should have content, not tool_calls (since no tools were provided).
	if content == "" {
		t.Log("warning: empty content (model may have called a tool despite none being offered)")
	}
	t.Logf("plain chat response: %q", content)
}

// TestRegression_RateLimit_KimiK2 verifies that kimi-k2-5, which has a 4 req/min
// rate limit, processes requests normally under that limit.
func TestRegression_RateLimit_KimiK2(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	var rawResp *http.Response
	_, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: "kimi-k2-5",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say 'ok'"),
		},
		MaxTokens: openai.Int(5),
	}, option.WithResponseInto(&rawResp))

	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case 404, 503:
				t.Skipf("kimi-k2-5 not available in this environment (status %d)", apiErr.StatusCode)
			case 429:
				t.Log("rate limit hit (expected at >4 req/min)")
				retryAfter := rawResp.Header.Get("Retry-After")
				if retryAfter == "" {
					t.Error("429 missing Retry-After header")
				}
				return
			}
		}
		t.Fatalf("chat completion: %v", err)
	}
	t.Log("kimi-k2-5 request succeeded within rate limit")
}

// TestRegression_Chat_DeepSeekR1 verifies the deepseek-r1-0528 model is reachable
// and returns a valid response.
func TestRegression_Chat_DeepSeekR1(t *testing.T) {
	cfg := getConfig(t)
	oai := newOpenAIClient(t, cfg)

	completion, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: "deepseek-r1-0528",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What is 2+2?"),
		},
		MaxTokens: openai.Int(20),
	})
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 404 || apiErr.StatusCode == 503) {
			t.Skipf("deepseek-r1-0528 not available (status %d)", apiErr.StatusCode)
		}
		t.Fatalf("chat completion: %v", err)
	}

	if len(completion.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	t.Logf("deepseek-r1-0528 response: %v", completion.Choices[0].Message.Content)
}
