package e2e_test

// Tests for /v1/responses + code_interpreter tool.
//
// Requires: router running with CODE_INTERPRETER_BASE_URL configured AND a
// backend model that supports the /v1/responses endpoint.
// Enable:   E2E_CI_AVAILABLE=true E2E_RESPONSES_API_AVAILABLE=true
// Optionally: E2E_RESPONSES_MODEL=<model> (defaults to E2E_MODEL)

import (
	"errors"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// TestResponsesCI_NonStreaming_BasicExecution verifies a single-turn /v1/responses
// call with code_interpreter: the model calls the tool, code runs, and the
// response output contains a code_interpreter_call item with status/container_id.
func TestResponsesCI_NonStreaming_BasicExecution(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responsesCIParams(cfg.ResponsesModel,
		"Use the code_interpreter tool. Run this Python code: print(6 * 7)"))
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	logJSON(t, "response", resp)

	ciItems := ciOutputItemsFromResponse(resp)
	if len(ciItems) == 0 {
		t.Fatal("expected at least one code_interpreter_call item in output")
	}
	for _, item := range ciItems {
		requireCIItemExecuted(t, item)
	}
}

// TestResponsesCI_NonStreaming_OutputLogs verifies that Python stdout from code execution
// is captured as an output when include=["code_interpreter_call.outputs"] is present.
func TestResponsesCI_NonStreaming_OutputLogs(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.ResponsesModel),
		Input: responseInput("Use code_interpreter to run: print('responses-e2e-marker')"),
		Tools: []responses.ToolUnionParam{codeInterpreterTool()},
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludableCodeInterpreterCallOutputs,
		},
		ToolChoice: ciToolChoiceForResponses(),
	})
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	ciItems := ciOutputItemsFromResponse(resp)
	if len(ciItems) == 0 {
		t.Fatal("no code_interpreter_call items in output")
	}

	item := ciItems[0]
	if item.Status != "completed" {
		t.Fatalf("expected status=completed, got %q", item.Status)
	}

	if len(item.Outputs) == 0 {
		t.Fatal("expected outputs in code_interpreter_call (include was set)")
	}

	markerFound := false
	for _, out := range item.Outputs {
		if logs := out.AsLogs(); logs.Logs != "" {
			t.Logf("logs: %q", logs.Logs)
			if strings.Contains(logs.Logs, "responses-e2e-marker") {
				markerFound = true
			}
		}
	}
	if !markerFound {
		t.Error("expected 'responses-e2e-marker' in code_interpreter_call outputs")
	}
}

// TestResponsesCI_NonStreaming_OutputsNotIncluded verifies that without
// include=["code_interpreter_call.outputs"], the outputs field is omitted.
func TestResponsesCI_NonStreaming_OutputsNotIncluded(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responsesCIParams(cfg.ResponsesModel,
		"Use the code_interpreter tool. Run this Python code: print(6 * 7)"))
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	ciItems := ciOutputItemsFromResponse(resp)
	if len(ciItems) == 0 {
		t.Fatal("no code_interpreter_call items in output")
	}

	for _, item := range ciItems {
		if len(item.Outputs) != 0 {
			t.Error("outputs should be omitted when include=code_interpreter_call.outputs is not set")
		}
	}
}

// TestResponsesCI_NonStreaming_MultiTurn verifies that the router loops: after the
// model calls code_interpreter, the router executes the code, feeds the result back
// as function_call_output, and the model produces a final text response.
// The output should contain both a code_interpreter_call and a message item.
func TestResponsesCI_NonStreaming_MultiTurn(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.ResponsesModel),
		Input: responseInput("Use code_interpreter to compute 17*19 and then explain the result."),
		Tools: []responses.ToolUnionParam{codeInterpreterTool()},
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludableCodeInterpreterCallOutputs,
		},
		ToolChoice: ciToolChoiceForResponses(),
	})
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	logJSON(t, "multi-turn response", resp)

	if len(resp.Output) == 0 {
		t.Fatal("expected output items")
	}

	var hasCI, hasMessage bool
	for _, item := range resp.Output {
		switch item.Type {
		case "code_interpreter_call":
			hasCI = true
			requireCIItemExecuted(t, item)
		case "message":
			hasMessage = true
		}
	}

	if !hasCI {
		t.Error("expected a code_interpreter_call item in output")
	}
	if !hasMessage {
		t.Log("note: no message item found — model may have stopped after code execution (model-dependent)")
	}
}

// TestResponsesCI_NonStreaming_MixedTools verifies that when both code_interpreter
// and a user-defined function are in the tools list and the model calls the
// user-defined function (not CI), the router still returns a complete response
// (mixed tools path terminates the loop immediately).
func TestResponsesCI_NonStreaming_MixedTools(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.ResponsesModel),
		Input: responseInput("Call the get_weather function for London."),
		Tools: []responses.ToolUnionParam{
			codeInterpreterTool(),
			{
				OfFunction: &responses.FunctionToolParam{
					Name:        "get_weather",
					Description: openai.String("Get the current weather for a city."),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"city": map[string]any{"type": "string"},
						},
						"required": []any{"city"},
					},
				},
			},
		},
		// Steer model toward the user tool so we exercise the mixed-tools exit path.
		ToolChoice: responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{
				Name: "get_weather",
			},
		},
	})
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	logJSON(t, "mixed tools response", resp)

	// Should have function_call for get_weather in output (not code_interpreter_call).
	var hasGetWeather bool
	for _, item := range resp.Output {
		if item.Type == "function_call" && item.Name == "get_weather" {
			hasGetWeather = true
		}
	}
	if !hasGetWeather {
		t.Log("model did not call get_weather (non-deterministic) — response still validated")
	}
	// Regardless of which tool was called, a valid response was returned.
}

// TestResponsesCI_NonStreaming_FailedExecution verifies that Python exceptions
// in code_interpreter are represented as status="failed" items in the output.
func TestResponsesCI_NonStreaming_FailedExecution(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.ResponsesModel),
		Input: responseInput("Use code_interpreter to run Python that raises: 1/0"),
		Tools: []responses.ToolUnionParam{codeInterpreterTool()},
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludableCodeInterpreterCallOutputs,
		},
		ToolChoice: ciToolChoiceForResponses(),
	})
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	ciItems := ciOutputItemsFromResponse(resp)
	if len(ciItems) == 0 {
		t.Fatal("no code_interpreter_call items in output")
	}

	item := ciItems[0]
	t.Logf("status=%s container_id=%s", item.Status, item.ContainerID)
	if item.Status != "failed" {
		t.Errorf("expected status=failed for division-by-zero code, got %q", item.Status)
	}
}

// TestResponsesCI_Streaming_EventSequence verifies the full SSE event sequence
// for a /v1/responses streaming call with code_interpreter.
func TestResponsesCI_Streaming_EventSequence(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	stream := rc.NewStreaming(background(), responsesCIParams(cfg.ResponsesModel,
		"Use the code_interpreter tool. Run this Python code: print(6 * 7)"))
	defer stream.Close()

	seen := make(map[string]bool)
	for stream.Next() {
		ev := stream.Current()
		seen[ev.Type] = true
		t.Logf("event: %s", ev.Type)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	requiredEventTypes := []string{
		"response.output_item.added",
		"response.code_interpreter_call_code.delta",
		"response.code_interpreter_call_code.done",
		"response.code_interpreter_call.in_progress",
		"response.code_interpreter_call.interpreting",
		"response.code_interpreter_call.completed",
		"response.output_item.done",
		"response.completed",
	}
	for _, want := range requiredEventTypes {
		if !seen[want] {
			t.Errorf("missing expected SSE event type %q", want)
		}
	}
}

// TestResponsesCI_Streaming_CodeEvents verifies that streaming CI events carry
// meaningful data: code.delta has a delta string, code.done has the full code,
// and the code_interpreter_call item in output_item.done has the expected fields.
func TestResponsesCI_Streaming_CodeEvents(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	stream := rc.NewStreaming(background(), responsesCIParams(cfg.ResponsesModel,
		"Use the code_interpreter tool. Run this Python code: print(6 * 7)"))
	defer stream.Close()

	var codeDelta, codeDone, itemDone, completed *responses.ResponseStreamEventUnion
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "response.code_interpreter_call_code.delta":
			e := ev
			codeDelta = &e
		case "response.code_interpreter_call_code.done":
			e := ev
			codeDone = &e
		case "response.output_item.done":
			if ev.Item.Type == "code_interpreter_call" {
				e := ev
				itemDone = &e
			}
		case "response.completed":
			e := ev
			completed = &e
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if codeDelta == nil {
		t.Error("missing response.code_interpreter_call_code.delta event")
	} else if codeDelta.Delta == "" {
		t.Error("code delta event has empty delta string")
	}

	if codeDone == nil {
		t.Error("missing response.code_interpreter_call_code.done event")
	} else if codeDone.Code == "" {
		t.Error("code done event has empty code string")
	}

	if itemDone != nil {
		item := itemDone.Item
		if item.Status == "" {
			t.Error("output_item.done code_interpreter_call item missing status")
		}
		if item.ContainerID == "" {
			t.Error("output_item.done code_interpreter_call item missing container_id")
		}
		t.Logf("code_interpreter_call item: status=%s container_id=%s", item.Status, item.ContainerID)
	}

	if completed != nil {
		t.Logf("response.completed usage: input_tokens=%d output_tokens=%d",
			completed.Response.Usage.InputTokens, completed.Response.Usage.OutputTokens)
	}
}

// TestResponsesCI_Streaming_SequenceNumbers verifies that emitted SSE events
// have monotonically increasing sequence_number fields.
func TestResponsesCI_Streaming_SequenceNumbers(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	stream := rc.NewStreaming(background(), responsesCIParams(cfg.ResponsesModel,
		"Use the code_interpreter tool. Run this Python code: print(6 * 7)"))
	defer stream.Close()

	var lastSeq int64 = -1
	for stream.Next() {
		ev := stream.Current()
		if ev.SequenceNumber == 0 {
			continue
		}
		if ev.SequenceNumber <= lastSeq {
			t.Errorf("sequence_number not monotonically increasing: %d after %d (event=%s)",
				ev.SequenceNumber, lastSeq, ev.Type)
		}
		lastSeq = ev.SequenceNumber
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if lastSeq < 0 {
		t.Error("no sequence_number fields found in any event")
	}
	t.Logf("last sequence_number: %d", lastSeq)
}

// TestResponsesCI_UsageAccumulatedAcrossIterations verifies that the usage field
// in the final response.completed event accumulates tokens from all upstream
// model calls (not just the last one).
func TestResponsesCI_UsageAccumulatedAcrossIterations(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	// Multi-turn: code execution + follow-up forces at least two model calls.
	stream := rc.NewStreaming(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.ResponsesModel),
		Input: responseInput("Use code_interpreter to compute the 10th Fibonacci number in Python, then summarize what you computed."),
		Tools: []responses.ToolUnionParam{codeInterpreterTool()},
		ToolChoice: ciToolChoiceForResponses(),
	})
	defer stream.Close()

	var finalUsage *responses.ResponseUsage
	for stream.Next() {
		ev := stream.Current()
		if ev.Type == "response.completed" {
			u := ev.Response.Usage
			finalUsage = &u
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if finalUsage == nil {
		t.Log("no usage data in response.completed event (model may not emit it)")
		return
	}

	t.Logf("accumulated usage: input_tokens=%d output_tokens=%d total_tokens=%d",
		finalUsage.InputTokens, finalUsage.OutputTokens, finalUsage.TotalTokens)
	if finalUsage.TotalTokens == 0 && finalUsage.InputTokens == 0 {
		t.Error("expected non-zero token count in accumulated usage")
	}
}

// TestResponsesCI_Error_MultipleCodeInterpreterTools verifies that providing two
// code_interpreter tools in one request is rejected.
func TestResponsesCI_Error_MultipleCodeInterpreterTools(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	c := newRouterClient(t, cfg)

	resp := c.postJSON(t, "/v1/responses", map[string]any{
		"model": cfg.ResponsesModel,
		"input": "hello",
		"tools": []any{
			map[string]any{"type": "code_interpreter"},
			map[string]any{"type": "code_interpreter"},
		},
		"stream": false,
	})
	requireStatus(t, resp, 400)
	result := decodeJSON(t, resp)
	requireErrorType(t, result, "invalid_request_error")
	t.Logf("correctly rejected duplicate tools: %v", result["error"])
}

// TestResponsesCI_PlainRequestNotIntercepted verifies that a plain /v1/responses
// request without code_interpreter in its tools list is not intercepted by the
// tool executor and routes normally.
func TestResponsesCI_PlainRequestNotIntercepted(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	rc := newResponsesClient(t, cfg)

	// No tools → NeedsHandling won't intercept, goes to normal routing.
	resp, err := rc.New(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.Model),
		Input: responseInput("Say hello."),
	})
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			if apiErr.StatusCode == 404 {
				return // backend does not support responses — expected
			}
			if apiErr.StatusCode == 400 {
				body := requireAPIError(t, err, 400)
				msg, _ := body["error"].(map[string]any)["message"].(string)
				if strings.Contains(msg, "code_interpreter") {
					t.Error("router incorrectly intercepted a request without code_interpreter tool")
				}
				return
			}
		}
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp
}

// TestResponsesCI_Error_UnsupportedContainerType verifies that a container type
// other than "auto" is rejected.
func TestResponsesCI_Error_UnsupportedContainerType(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	c := newRouterClient(t, cfg)

	resp := c.postJSON(t, "/v1/responses", map[string]any{
		"model": cfg.ResponsesModel,
		"input": "hello",
		"tools": []any{
			map[string]any{
				"type": "code_interpreter",
				"container": map[string]any{
					"type": "persistent-volume",
				},
			},
		},
		"stream": false,
	})
	requireStatus(t, resp, 400)
	result := decodeJSON(t, resp)
	requireErrorType(t, result, "invalid_request_error")
}

// TestResponsesCI_ToolChoice_CodeInterpreter verifies that tool_choice={type:"code_interpreter"}
// is rewritten to force function calling and the model uses the tool.
func TestResponsesCI_ToolChoice_CodeInterpreter(t *testing.T) {
	cfg := getConfig(t)
	cfg.requireCI(t)
	cfg.requireResponsesAPI(t)
	rc := newResponsesClient(t, cfg)

	resp, err := rc.New(background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.ResponsesModel),
		Input: responseInput("Use the code_interpreter tool. Run this Python code: print(3+3)"),
		Tools: []responses.ToolUnionParam{codeInterpreterTool()},
		ToolChoice: responses.ResponseNewParamsToolChoiceUnion{
			OfHostedTool: &responses.ToolChoiceTypesParam{
				Type: "code_interpreter",
			},
		},
	})
	if err != nil {
		t.Fatalf("responses: %v", err)
	}

	ciItems := ciOutputItemsFromResponse(resp)
	if len(ciItems) == 0 {
		t.Error("expected code_interpreter_call when tool_choice forces code_interpreter")
	} else {
		t.Logf("code_interpreter_call: status=%s", ciItems[0].Status)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// responsesCIParams builds ResponseNewParams for a code_interpreter test.
func responsesCIParams(model, input string) responses.ResponseNewParams {
	return responses.ResponseNewParams{
		Model:      shared.ResponsesModel(model),
		Input:      responseInput(input),
		Tools:      []responses.ToolUnionParam{codeInterpreterTool()},
		ToolChoice: ciToolChoiceForResponses(),
	}
}
