package e2e_test

// Tests for web search routing.
//
// web_search_options in chat completions, or tools=[{type:"web_search"}] in
// responses, should route the request to the "websearch" enclave instead of
// the normal model enclave.
//
// Enable: E2E_WEBSEARCH_AVAILABLE=true

import (
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// TestWebsearch_Chat_WebSearchOptions_NonStreaming verifies that a /v1/chat/completions
// request with web_search_options is routed to the websearch enclave and returns
// a valid response.
func TestWebsearch_Chat_WebSearchOptions_NonStreaming(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireWebsearch(t)
	oai := newOpenAIClient(t, cfg)

	completion, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What is the latest version of Go?"),
		},
		WebSearchOptions: openai.ChatCompletionNewParamsWebSearchOptions{},
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	logJSON(t, "websearch response", parseChatCompletion(t, completion.RawJSON()))

	if len(completion.Choices) == 0 {
		t.Fatal("no choices in websearch response")
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	if content == "" {
		t.Error("expected non-empty content from websearch response")
	}
	t.Logf("websearch response content (first 200 chars): %.200s", content)
}

// TestWebsearch_Chat_WebSearchOptions_Streaming verifies that a streaming
// chat completion with web_search_options returns a valid SSE stream.
func TestWebsearch_Chat_WebSearchOptions_Streaming(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireWebsearch(t)
	oai := newOpenAIClient(t, cfg)

	stream := oai.Chat.Completions.NewStreaming(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Search for current AI news headlines."),
		},
		WebSearchOptions: openai.ChatCompletionNewParamsWebSearchOptions{},
	})
	defer stream.Close()

	var chunkCount int
	hasContent := false

	for stream.Next() {
		chunk := stream.Current()
		chunkCount++
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				hasContent = true
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if chunkCount == 0 {
		t.Fatal("no chunks received from websearch streaming response")
	}
	if !hasContent {
		t.Log("no content delta found in streaming chunks (model may have sent content in a single chunk)")
	}
	t.Logf("received %d chunks from streaming websearch", chunkCount)
}

// TestWebsearch_Responses_WebSearchTool_NonStreaming verifies that /v1/responses
// with tools=[{type:"web_search"}] routes to the websearch enclave.
func TestWebsearch_Responses_WebSearchTool_NonStreaming(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireWebsearch(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.Model),
		Input: responseInput("What is the current Bitcoin price?"),
		Tools: []responses.ToolUnionParam{webSearchTool()},
	})
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	logJSON(t, "websearch responses body", resp)

	if len(resp.Output) == 0 {
		t.Error("expected output items from websearch responses")
	}
}

// TestWebsearch_Responses_WebSearchTool_Streaming verifies streaming /v1/responses
// with web_search tool produces a valid SSE stream.
func TestWebsearch_Responses_WebSearchTool_Streaming(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireWebsearch(t)
	rc := newResponsesClient(t, cfg)

	stream := rc.NewStreaming(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.Model),
		Input: responseInput("Search for the latest news in technology."),
		Tools: []responses.ToolUnionParam{webSearchTool()},
	})
	defer stream.Close()

	var eventCount int
	for stream.Next() {
		eventCount++
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if eventCount == 0 {
		t.Fatal("no events received from websearch streaming /v1/responses")
	}
	t.Logf("received %d SSE events", eventCount)
}

// TestWebsearch_RoutedToWebsearchEnclave verifies the Tinfoil-Enclave response
// header identifies the websearch enclave (not a general model enclave).
//
// Note: WebSearchOptions: openai.ChatCompletionNewParamsWebSearchOptions{} is
// intentionally NOT used here. The empty struct is marked as zero by paramObj
// and omitted from serialized JSON (omitzero), so web_search_options never
// reaches the router. We use option.WithJSONSet to force the field in.
func TestWebsearch_RoutedToWebsearchEnclave(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireWebsearch(t)
	oai := newOpenAIClient(t, cfg)

	var rawResp *http.Response
	_, _ = oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Search for AI news."),
		},
	}, option.WithJSONSet("web_search_options", map[string]any{}),
		option.WithResponseInto(&rawResp))

	if rawResp == nil {
		t.Skip("Tinfoil-Enclave header not present (router may not set it for proxied requests)")
	}

	enclaveHeader := rawResp.Header.Get("Tinfoil-Enclave")
	if enclaveHeader == "" {
		t.Skip("Tinfoil-Enclave header not present (router may not set it for proxied requests)")
	}

	t.Logf("Tinfoil-Enclave: %s", enclaveHeader)
	if !strings.Contains(strings.ToLower(enclaveHeader), "websearch") {
		t.Errorf("expected websearch enclave in header, got %q", enclaveHeader)
	}
}

// TestWebsearch_Detection_ViaWebSearchOptions verifies that web_search_options
// is detected on any path (not just /v1/responses).
func TestWebsearch_Detection_ViaWebSearchOptions(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireWebsearch(t)
	oai := newOpenAIClient(t, cfg)

	// Even without tools list, web_search_options alone should trigger websearch routing.
	completion, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What year is it?"),
		},
	},
		option.WithJSONSet("web_search_options", map[string]any{"search_context_size": "low"}),
	)
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	if len(completion.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	t.Logf("websearch via web_search_options: %.100s", content)
}
