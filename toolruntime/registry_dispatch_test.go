package toolruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinfoilsh/confidential-model-router/toolruntime/citations"
)

// TestExecuteRouterToolCallDispatchesToCorrectProfile pins the
// multi-profile end-to-end contract: a registry built from two
// profiles that each advertise disjoint tools must route every
// executeRouterToolCall invocation to the session that owns that
// tool. Failure modes this guards:
//   - silently invoking the wrong session and returning its output
//   - caching the last-used session across calls
//   - dropping calls that don't hit the first-registered profile
func TestExecuteRouterToolCallDispatchesToCorrectProfile(t *testing.T) {
	searchSession := startTestMCPServer(t, "websearch", "search")
	fakeSession := startTestMCPServer(t, "fake-server", "fake_tool")

	profiles := []Profile{
		{Name: "web_search", ToolServerModel: "websearch"},
		{Name: "fake_profile", ToolServerModel: "fake-server"},
	}
	dial := dialFromMap(map[string]*mcp.ClientSession{
		"web_search":   searchSession,
		"fake_profile": fakeSession,
	})

	registry, err := buildSessionRegistry(context.Background(), profiles, dial)
	if err != nil {
		t.Fatalf("buildSessionRegistry: %v", err)
	}
	defer registry.CloseAll()

	cases := []struct {
		name     string
		callName string
		wantSub  string
	}{
		{
			name:     "search routes to websearch server",
			callName: routerSearchToolName,
			wantSub:  "websearch:search",
		},
		{
			name:     "fake_tool routes to fake-server",
			callName: "fake_tool",
			wantSub:  "fake-server:fake_tool",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			citState := &citations.State{NextIndex: 1}
			toolCalls := &toolCallLog{}
			output := executeRouterToolCall(
				context.Background(),
				registry,
				toolCall{name: tc.callName, arguments: map[string]any{}},
				webSearchOptions{},
				nil,
				citState,
				toolCalls,
				"test",
				"",
			)
			if !strings.Contains(output, tc.wantSub) {
				t.Fatalf("executeRouterToolCall(%q) output = %q, want substring %q", tc.callName, output, tc.wantSub)
			}
		})
	}
}

func TestExecuteRouterToolCallPreservesStructuredSearchMetadata(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "websearch", Version: "v1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        routerSearchToolName,
		Description: "test search tool",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "search result"}},
			StructuredContent: map[string]any{
				"results": []any{map[string]any{
					"url":            "https://example.com/result",
					"title":          "Example result",
					"content":        "Relevant excerpt.",
					"published_date": "2026-08-10",
					"author":         "Alex Example",
					"favicon":        "https://example.com/favicon.ico",
				}},
			},
		}, nil
	})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() {
		if err := server.Run(ctx, serverTransport); err != nil && ctx.Err() == nil {
			t.Logf("mcp server stopped: %v", err)
		}
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	registry, err := buildSessionRegistry(ctx, []Profile{{Name: "web_search", ToolServerModel: "websearch"}}, dialFromMap(map[string]*mcp.ClientSession{"web_search": session}))
	if err != nil {
		t.Fatalf("buildSessionRegistry: %v", err)
	}
	defer registry.CloseAll()

	toolCalls := &toolCallLog{}
	executeRouterToolCall(
		ctx,
		registry,
		toolCall{name: routerSearchToolName, arguments: map[string]any{"query": "example"}},
		webSearchOptions{},
		nil,
		&citations.State{NextIndex: 1},
		toolCalls,
		"test",
		"",
	)

	items := buildWebSearchCallOutputItems(toolCalls.list(), true)
	item, _ := items[0].(map[string]any)
	action, _ := item["action"].(map[string]any)
	sources, _ := action["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("expected one action source, got %#v", action)
	}
	source, _ := sources[0].(map[string]any)
	if source["title"] != "Example result" || source["snippet"] != "Relevant excerpt." || source["published_date"] != "2026-08-10" || source["author"] != "Alex Example" {
		t.Fatalf("structured source metadata was not preserved: %#v", source)
	}
	if _, present := source["favicon"]; present {
		t.Fatalf("favicon must not be returned: %#v", source)
	}

	streamer, recorder := newTestResponsesStreamerForSpecEvents(t)
	streamer.includeActionSources = true
	_, _, err = executeToolWithProgress(
		ctx,
		registry,
		&citations.State{NextIndex: 1},
		&responsesToolProgressEmitter{streamer: streamer},
		toolCall{name: routerSearchToolName, arguments: map[string]any{"query": "example"}},
	)
	if err != nil {
		t.Fatalf("executeToolWithProgress: %v", err)
	}
	streamBody := recorder.Body.String()
	for _, field := range []string{
		`"title":"Example result"`,
		`"snippet":"Relevant excerpt."`,
		`"published_date":"2026-08-10"`,
		`"author":"Alex Example"`,
	} {
		if !strings.Contains(streamBody, field) {
			t.Fatalf("streaming terminal item missing %s: %s", field, streamBody)
		}
	}
}

// TestExecuteRouterToolCallUnknownToolReturnsHumanizedError pins
// the defensive path for the registry-mismatch programming-error
// case: executeRouterToolCall must produce a deterministic text
// payload the upstream model can read rather than panicking, and
// must still record the failure on citations so downstream
// annotation counters stay consistent.
func TestExecuteRouterToolCallUnknownToolReturnsHumanizedError(t *testing.T) {
	searchSession := startTestMCPServer(t, "websearch", "search")
	profiles := []Profile{{Name: "web_search", ToolServerModel: "websearch"}}
	dial := dialFromMap(map[string]*mcp.ClientSession{"web_search": searchSession})

	registry, err := buildSessionRegistry(context.Background(), profiles, dial)
	if err != nil {
		t.Fatalf("buildSessionRegistry: %v", err)
	}
	defer registry.CloseAll()

	citState := &citations.State{NextIndex: 1}
	toolCalls := &toolCallLog{}
	output := executeRouterToolCall(
		context.Background(),
		registry,
		toolCall{name: "nonexistent_tool", arguments: map[string]any{}},
		webSearchOptions{},
		nil,
		citState,
		toolCalls,
		"test",
		"",
	)
	if output == "" {
		t.Fatalf("expected non-empty humanized error output, got empty string")
	}
	if len(toolCalls.records) != 1 {
		t.Fatalf("expected 1 recorded tool call, got %d", len(toolCalls.records))
	}
	if toolCalls.records[0].errorReason == "" {
		t.Errorf("expected recorded errorReason, got empty string")
	}
}
