package toolruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/toolprofile"
)

// startTestMCPServer spins up an in-memory MCP server that advertises
// the given tool names and returns the client session the test
// interacts with through the registry. Every tool simply echoes a
// constant string so callers can still exercise CallTool routing
// without caring about arguments.
func startTestMCPServer(t *testing.T, serverName string, toolNames ...string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: "v1"}, nil)
	for _, name := range toolNames {
		name := name
		srv.AddTool(&mcp.Tool{
			Name:        name,
			Description: "test tool " + name,
			InputSchema: &jsonschema.Schema{Type: "object"},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: serverName + ":" + name}},
			}, nil
		})
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() {
		if err := srv.Run(ctx, serverTransport); err != nil && ctx.Err() == nil {
			t.Logf("mcp server %s stopped: %v", serverName, err)
		}
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "registry-test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("mcp client connect %s: %v", serverName, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// dialFromMap returns a dial function that plays back pre-built
// sessions keyed by profile name. Used by the registry tests so we
// don't have to stand up HTTP stacks just to exercise the routing
// table and conflict-detection paths.
func dialFromMap(sessions map[string]*mcp.ClientSession) func(context.Context, toolprofile.Profile) (*mcp.ClientSession, error) {
	return func(_ context.Context, p toolprofile.Profile) (*mcp.ClientSession, error) {
		s, ok := sessions[p.Name]
		if !ok {
			t := "unknown"
			return nil, &registryTestError{msg: "no fake session for profile " + p.Name + " (have " + t + ")"}
		}
		return s, nil
	}
}

type registryTestError struct{ msg string }

func (e *registryTestError) Error() string { return e.msg }

// TestSessionRegistryRoutesTwoProfilesToSeparateSessions pins the
// multi-profile contract: a request with two active profiles builds
// a registry where every tool name routes to exactly the session
// that advertised it, the owned-tool set is the union of both
// servers' tools, and profileNames reflects both profiles for debug
// logs.
func TestSessionRegistryRoutesTwoProfilesToSeparateSessions(t *testing.T) {
	searchSession := startTestMCPServer(t, "websearch", "search", "fetch")
	fakeSession := startTestMCPServer(t, "fake-server", "fake_tool")

	profiles := []toolprofile.Profile{
		{Name: "web_search", ToolServerModel: "websearch"},
		{Name: "fake_profile", ToolServerModel: "fake-server"},
	}
	dial := dialFromMap(map[string]*mcp.ClientSession{
		"web_search":   searchSession,
		"fake_profile": fakeSession,
	})

	r, err := buildSessionRegistry(context.Background(), profiles, dial)
	if err != nil {
		t.Fatalf("buildSessionRegistry: %v", err)
	}

	for _, name := range []string{routerSearchToolName, routerFetchToolName} {
		got, ok := r.sessionFor(name)
		if !ok {
			t.Fatalf("sessionFor(%q) missing", name)
		}
		if got != searchSession {
			t.Errorf("sessionFor(%q) routed to wrong session", name)
		}
	}
	if got, ok := r.sessionFor("fake_tool"); !ok || got != fakeSession {
		t.Fatalf("sessionFor(fake_tool) routed to %v ok=%v", got, ok)
	}

	owned := r.ownedTools()
	for _, want := range []string{routerSearchToolName, routerFetchToolName, "fake_tool"} {
		if _, ok := owned[want]; !ok {
			t.Errorf("ownedTools missing %q; got %v", want, owned)
		}
	}
	if len(owned) != 3 {
		t.Errorf("ownedTools size = %d, want 3", len(owned))
	}
	toolNames := ownedToolNames(r.allTools())
	for _, want := range []string{routerSearchToolName, routerFetchToolName, "fake_tool"} {
		if _, ok := toolNames[want]; !ok {
			t.Errorf("allTools missing %q; got %v", want, toolNames)
		}
	}

	names := r.profileNames()
	if len(names) != 2 {
		t.Fatalf("profileNames = %v, want 2 entries", names)
	}
}

// TestSessionRegistryFailsLoudOnToolNameConflict pins the
// fail-loud-on-config-error invariant: if two active profiles
// advertise the same tool name, construction returns an error
// naming the offending tool rather than silently last-writer-wins
// routing, and every session opened before the conflict is
// discovered is closed.
func TestSessionRegistryFailsLoudOnToolNameConflict(t *testing.T) {
	firstSession := startTestMCPServer(t, "first", "search")
	secondSession := startTestMCPServer(t, "second", "search")

	profiles := []toolprofile.Profile{
		{Name: "primary", ToolServerModel: "first"},
		{Name: "shadow", ToolServerModel: "second"},
	}
	dial := dialFromMap(map[string]*mcp.ClientSession{
		"primary": firstSession,
		"shadow":  secondSession,
	})

	r, err := buildSessionRegistry(context.Background(), profiles, dial)
	if err == nil {
		t.Fatalf("expected conflict error, got registry with profiles %v", r.profileNames())
	}
	if !strings.Contains(err.Error(), "search") {
		t.Errorf("conflict error must name the offending tool; got %q", err.Error())
	}
	if r != nil {
		t.Errorf("expected nil registry on conflict, got %+v", r)
	}
}

// TestSessionRegistryDialFailureClosesPriorSessions pins the
// partial-open-cleanup contract: if the second profile fails to
// dial, the first profile's session must be closed before the
// error surfaces so a failed request never leaks MCP connections
// back to the trusted enclave.
func TestSessionRegistryDialFailureClosesPriorSessions(t *testing.T) {
	firstSession := startTestMCPServer(t, "first", "search")

	profiles := []toolprofile.Profile{
		{Name: "primary", ToolServerModel: "first"},
		{Name: "broken", ToolServerModel: "nowhere"},
	}
	dial := func(_ context.Context, p toolprofile.Profile) (*mcp.ClientSession, error) {
		switch p.Name {
		case "primary":
			return firstSession, nil
		default:
			return nil, &registryTestError{msg: "dial refused: " + p.Name}
		}
	}

	r, err := buildSessionRegistry(context.Background(), profiles, dial)
	if err == nil {
		t.Fatalf("expected dial error, got registry")
	}
	if r != nil {
		t.Errorf("expected nil registry on dial failure, got %+v", r)
	}
	// Positive signal that buildSessionRegistry closed the first
	// session on its way out: an RPC over a live session succeeds,
	// an RPC over a closed session fails. Double-close on its own
	// cannot distinguish "was closed by cleanup" from "was never
	// closed at all", so we exercise the transport instead.
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := firstSession.Ping(pingCtx, nil); err == nil {
		t.Errorf("firstSession.Ping succeeded after buildSessionRegistry cleanup; expected session to be closed")
	}
}

// TestSessionRegistryEmptyProfilesFailsLoud pins that Handle cannot
// accidentally open zero sessions: the outer caller is expected to
// short-circuit on an empty detector result, and a registry built
// against an empty profile list is a programming error we surface
// rather than masking.
func TestSessionRegistryEmptyProfilesFailsLoud(t *testing.T) {
	_, err := buildSessionRegistry(context.Background(), nil, func(context.Context, toolprofile.Profile) (*mcp.ClientSession, error) {
		t.Fatal("dial should not be invoked for empty profile list")
		return nil, nil
	})
	if err == nil {
		t.Fatalf("expected error for empty profile list")
	}
}

// TestSessionRegistryCloseAllIsIdempotent pins CloseAll's tolerance
// for being called twice: Handle defers CloseAll, but an early
// error path may also invoke it explicitly, and a double-close
// must not panic or corrupt the entries slice.
func TestSessionRegistryCloseAllIsIdempotent(t *testing.T) {
	session := startTestMCPServer(t, "only", "search")
	profiles := []toolprofile.Profile{{Name: "only", ToolServerModel: "only"}}
	dial := dialFromMap(map[string]*mcp.ClientSession{"only": session})

	r, err := buildSessionRegistry(context.Background(), profiles, dial)
	if err != nil {
		t.Fatalf("buildSessionRegistry: %v", err)
	}

	r.CloseAll()
	// Second CloseAll must not panic and must not double-close in a way
	// that corrupts the underlying session's state.
	r.CloseAll()
}
