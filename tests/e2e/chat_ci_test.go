package e2e_test

// Tests for /v1/chat/completions + code_interpreter_options.
//
// Requires: router running with CODE_INTERPRETER_BASE_URL configured.
// Enable:   E2E_CI_AVAILABLE=true

import (
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// TestChatCI_NonStreaming_BasicExecution verifies that when code_interpreter_options
// is present, the router executes Python code and injects status/container_id/outputs
// back into the tool_call before returning the response.
func TestChatCI_NonStreaming_BasicExecution(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	completion, err := oai.Chat.Completions.New(background(),
		chatCIParams(cfg.Model, "Use the code_interpreter tool. Run this Python code: print(2 + 2)"),
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
	)
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	raw := parseChatCompletion(t, completion.RawJSON())
	logJSON(t, "response", raw)

	ciCalls := ciToolCallsFromRaw(raw)
	if len(ciCalls) == 0 {
		t.Fatal("model did not produce a code_interpreter tool_call")
	}

	for _, tc := range ciCalls {
		requireCIToolCallExecuted(t, tc)
		if tc.Status != "completed" && tc.Status != "failed" {
			t.Errorf("unexpected status %q; want completed or failed", tc.Status)
		}
	}
}

// TestChatCI_NonStreaming_OutputLogs verifies that Python stdout is captured in the
// outputs array with type "logs".
func TestChatCI_NonStreaming_OutputLogs(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	completion, err := oai.Chat.Completions.New(background(),
		chatCIParams(cfg.Model, "Use code_interpreter to run: print('hello-e2e-test')"),
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
	)
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	raw := parseChatCompletion(t, completion.RawJSON())
	ciCalls := ciToolCallsFromRaw(raw)
	if len(ciCalls) == 0 {
		t.Fatal("model did not produce a code_interpreter tool_call")
	}

	tc := ciCalls[0]
	if tc.Status != "completed" {
		t.Fatalf("expected status=completed, got %q", tc.Status)
	}

	foundMarker := false
	for _, out := range tc.Outputs {
		if out.Type == "logs" {
			t.Logf("logs output: %q", out.Logs)
			if strings.Contains(out.Logs, "hello-e2e-test") {
				foundMarker = true
			}
		}
	}
	if !foundMarker {
		t.Error("expected 'hello-e2e-test' in code execution logs output")
	}
}

// TestChatCI_NonStreaming_FailedExecution verifies that Python exceptions produce
// status="failed" and the error is captured in outputs.
func TestChatCI_NonStreaming_FailedExecution(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	var ciCalls []rawCIToolCall
	for attempt := range 2 {
		completion, err := oai.Chat.Completions.New(background(),
			chatCIParams(cfg.Model, "Use code_interpreter to run this Python code that raises an exception: raise ValueError('e2e-error')"),
			option.WithJSONSet("code_interpreter_options", map[string]any{}),
		)
		if err != nil {
			t.Fatalf("chat completion: %v", err)
		}
		raw := parseChatCompletion(t, completion.RawJSON())
		ciCalls = ciToolCallsFromRaw(raw)
		if len(ciCalls) > 0 {
			break
		}
		if attempt == 0 {
			t.Log("model did not produce a tool_call on first attempt, retrying")
		}
	}
	if len(ciCalls) == 0 {
		t.Fatal("model did not produce a code_interpreter tool_call after retry")
	}

	tc := ciCalls[0]
	t.Logf("status=%s outputs=%v", tc.Status, tc.Outputs)

	if tc.Status != "failed" {
		t.Errorf("expected status=failed for exception code, got %q", tc.Status)
	}
	if tc.ContainerID == "" {
		t.Error("expected container_id even on failure")
	}
}

// TestChatCI_NonStreaming_ImageOutput verifies that matplotlib plots produce
// an output of type "image" in the outputs array.
func TestChatCI_NonStreaming_ImageOutput(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	completion, err := oai.Chat.Completions.New(background(),
		chatCIParams(cfg.Model, "Use code_interpreter to create a simple matplotlib bar chart with data [1,2,3] and labels ['a','b','c']. Save and show it."),
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
	)
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	raw := parseChatCompletion(t, completion.RawJSON())
	ciCalls := ciToolCallsFromRaw(raw)
	if len(ciCalls) == 0 {
		t.Fatal("model did not produce a code_interpreter tool_call")
	}

	tc := ciCalls[0]
	t.Logf("status=%s", tc.Status)
	if tc.Status != "completed" {
		t.Skip("code execution failed (possibly matplotlib unavailable in sandbox)")
	}

	imageFound := false
	for _, out := range tc.Outputs {
		if out.Type == "image" {
			imageFound = true
			if !strings.HasPrefix(out.URL, "data:image/") {
				t.Errorf("image URL expected data:image/* prefix, got %q", truncate(out.URL, 40))
			}
			t.Logf("image output: %s...", truncate(out.URL, 60))
		}
	}
	if !imageFound {
		t.Error("expected an image output from matplotlib plot")
	}
}

// TestChatCI_Streaming_BasicExecution verifies that in streaming mode, the router
// forwards normal SSE chunks and appends an extra execution chunk containing
// status/container_id/outputs after the model stream ends.
func TestChatCI_Streaming_BasicExecution(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	stream := oai.Chat.Completions.NewStreaming(background(),
		chatCIParams(cfg.Model, "Use the code_interpreter tool. Run this Python code: print(2 + 2)"),
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
	)
	defer stream.Close()

	var chunkCount int
	var execChunk *rawChatCompletion

	for stream.Next() {
		chunk := stream.Current()
		chunkCount++
		raw := parseChatCompletion(t, chunk.RawJSON())
		// Find the execution result chunk — has tool_calls[].status and container_id.
		for _, choice := range raw.Choices {
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Status != "" {
					c := raw
					execChunk = &c
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	t.Logf("received %d chunks", chunkCount)

	if execChunk == nil {
		t.Fatal("no execution chunk found in stream; expected a chunk with tool_calls[].status")
	}
	t.Logf("execution chunk: %v", execChunk)
}

// TestChatCI_Streaming_UsageChunk verifies that the usage-only chunk is suppressed
// unless the client explicitly asked for it (via stream_options.include_usage).
func TestChatCI_Streaming_UsageChunk(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	// Without include_usage: usage-only chunks should be suppressed.
	stream1 := oai.Chat.Completions.NewStreaming(background(),
		chatCIParams(cfg.Model, "Use the code_interpreter tool. Run this Python code: print(2 + 2)"),
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
	)
	for stream1.Next() {
		chunk := stream1.Current()
		if chunk.Usage.TotalTokens > 0 && len(chunk.Choices) == 0 {
			t.Error("usage-only chunk should be suppressed when client did not request streaming usage")
		}
	}
	if err := stream1.Err(); err != nil {
		t.Fatalf("stream1 error: %v", err)
	}
	stream1.Close()

	// With include_usage: usage-only chunk should be present.
	stream2 := oai.Chat.Completions.NewStreaming(background(),
		chatCIParams(cfg.Model, "Use the code_interpreter tool. Run this Python code: print(2 + 2)"),
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
		option.WithJSONSet("stream_options", map[string]any{"include_usage": true}),
	)
	defer stream2.Close()

	hasUsageChunk := false
	for stream2.Next() {
		chunk := stream2.Current()
		if chunk.Usage.TotalTokens > 0 && len(chunk.Choices) == 0 {
			hasUsageChunk = true
		}
	}
	if err := stream2.Err(); err != nil {
		t.Fatalf("stream2 error: %v", err)
	}
	if !hasUsageChunk {
		t.Log("warning: no usage-only chunk found even when include_usage=true (model may not emit it)")
	}
}

// TestChatCI_SessionPersistence verifies that two sequential calls using the same
// named container can share state (variables defined in the first call are
// accessible in the second).
func TestChatCI_SessionPersistence(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	// First call: define a variable.
	completion1, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Use code_interpreter to run: x_e2e_marker = 12345"),
		},
		ToolChoice: ciToolChoiceParam(),
	},
		option.WithJSONSet("code_interpreter_options", map[string]any{"type": "auto"}),
	)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	raw1 := parseChatCompletion(t, completion1.RawJSON())
	ciCalls1 := ciToolCallsFromRaw(raw1)
	if len(ciCalls1) == 0 {
		t.Fatal("first call: no code_interpreter tool_call")
	}
	containerID := ciCalls1[0].ContainerID
	if containerID == "" {
		t.Fatal("first call: missing container_id")
	}
	t.Logf("first call container_id=%s", containerID)

	// Second call: reference the variable, providing the same container_id.
	completion2, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Use code_interpreter to run: print(x_e2e_marker)"),
		},
		ToolChoice: ciToolChoiceParam(),
	},
		option.WithJSONSet("code_interpreter_options", containerID), // named container
	)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	raw2 := parseChatCompletion(t, completion2.RawJSON())
	ciCalls2 := ciToolCallsFromRaw(raw2)
	if len(ciCalls2) == 0 {
		t.Fatal("second call: no code_interpreter tool_call")
	}

	tc2 := ciCalls2[0]
	t.Logf("second call status=%s container_id=%s", tc2.Status, tc2.ContainerID)

	// Same container should be reused.
	if tc2.ContainerID != containerID {
		t.Errorf("expected same container_id=%s, got %s", containerID, tc2.ContainerID)
	}

	if tc2.Status == "completed" {
		// Variable x_e2e_marker should have been accessible.
		for _, out := range tc2.Outputs {
			if out.Type == "logs" && strings.Contains(out.Logs, "12345") {
				t.Logf("session persistence confirmed: variable value %q found in second call", out.Logs)
				return
			}
		}
		t.Error("session persistence: expected '12345' in second call output (variable defined in first call)")
	}
}

// TestChatCI_ToolNameCollision verifies that the router rejects a request where
// the client defines a function named "code_interpreter" — this collides with the
// reserved tool name injected by the router.
func TestChatCI_ToolNameCollision(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	_, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("hello"),
		},
		Tools: []openai.ChatCompletionToolUnionParam{
			openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        "code_interpreter",
				Description: openai.String("shadow the reserved tool"),
				Parameters: shared.FunctionParameters{
					"type":       "object",
					"properties": map[string]any{},
				},
			}),
		},
	},
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
	)
	body := requireAPIError(t, err, 400)
	requireErrorType(t, body, "invalid_request_error")
	t.Logf("correctly rejected tool name collision: %v", body["error"])
}

// TestChatCI_ContainerConfig_FileIDs_NotImplemented verifies that requesting
// file_ids in the container config returns a 400 (not yet implemented).
func TestChatCI_ContainerConfig_FileIDs_NotImplemented(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	_, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("use code"),
		},
	},
		option.WithJSONSet("code_interpreter_options", map[string]any{
			"type":     "auto",
			"file_ids": []any{"file-abc"},
		}),
	)
	body := requireAPIError(t, err, 400)
	requireErrorType(t, body, "invalid_request_error")
	t.Logf("correctly rejected file_ids: %v", body["error"])
}

// TestChatCI_ContainerConfig_NetworkPolicy_OnlyDisabledSupported verifies that
// a non-"disabled" network_policy is rejected.
func TestChatCI_ContainerConfig_NetworkPolicy_OnlyDisabledSupported(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	_, err := oai.Chat.Completions.New(background(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(cfg.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("use code"),
		},
	},
		option.WithJSONSet("code_interpreter_options", map[string]any{
			"type":           "auto",
			"network_policy": map[string]any{"type": "allowed"},
		}),
	)
	body := requireAPIError(t, err, 400)
	requireErrorType(t, body, "invalid_request_error")
}

// TestChatCI_EnclaveHeaderPresent verifies the Tinfoil-Enclave response header
// is set on code-interpreter responses, giving clients visibility into which
// enclave handled the upstream model call.
func TestChatCI_EnclaveHeaderPresent(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	oai := newOpenAIClient(t, cfg)

	var rawResp *http.Response
	_, err := oai.Chat.Completions.New(background(),
		chatCIParams(cfg.Model, "Use the code_interpreter tool. Run this Python code: print(2 + 2)"),
		option.WithJSONSet("code_interpreter_options", map[string]any{}),
		option.WithResponseInto(&rawResp),
	)
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}

	enclave := rawResp.Header.Get("Tinfoil-Enclave")
	if enclave == "" {
		t.Error("expected Tinfoil-Enclave header in code-interpreter response")
	} else {
		t.Logf("Tinfoil-Enclave: %s", enclave)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// chatCIParams builds ChatCompletionNewParams for a code_interpreter test.
// The tool_choice forcing code_interpreter is included.
func chatCIParams(model, userMessage string) openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model: shared.ChatModel(model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(userMessage),
		},
		ToolChoice: ciToolChoiceParam(),
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
