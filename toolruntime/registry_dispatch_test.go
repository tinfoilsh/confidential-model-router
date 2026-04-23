package toolruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/toolprofile"
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

	profiles := []toolprofile.Profile{
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
			citations := &citationState{nextIndex: 1}
			output := executeRouterToolCall(
				context.Background(),
				registry,
				toolCall{name: tc.callName, arguments: map[string]any{}},
				webSearchOptions{},
				nil,
				citations,
				"test",
				"",
			)
			if !strings.Contains(output, tc.wantSub) {
				t.Fatalf("executeRouterToolCall(%q) output = %q, want substring %q", tc.callName, output, tc.wantSub)
			}
		})
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
	profiles := []toolprofile.Profile{{Name: "web_search", ToolServerModel: "websearch"}}
	dial := dialFromMap(map[string]*mcp.ClientSession{"web_search": searchSession})

	registry, err := buildSessionRegistry(context.Background(), profiles, dial)
	if err != nil {
		t.Fatalf("buildSessionRegistry: %v", err)
	}
	defer registry.CloseAll()

	citations := &citationState{nextIndex: 1}
	output := executeRouterToolCall(
		context.Background(),
		registry,
		toolCall{name: "nonexistent_tool", arguments: map[string]any{}},
		webSearchOptions{},
		nil,
		citations,
		"test",
		"",
	)
	if output == "" {
		t.Fatalf("expected non-empty humanized error output, got empty string")
	}
	if len(citations.toolCalls) != 1 {
		t.Fatalf("expected 1 recorded tool call, got %d", len(citations.toolCalls))
	}
	if citations.toolCalls[0].errorReason == "" {
		t.Errorf("expected recorded errorReason, got empty string")
	}
}
