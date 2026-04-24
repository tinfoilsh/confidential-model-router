package toolruntime

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tinfoilsh/confidential-model-router/toolprofile"
)

// sessionRegistry holds the set of MCP sessions a single request has
// opened, one per active toolprofile.Profile, and routes each
// router-owned tool call to the session whose server advertised that
// tool.
//
// Invariants the registry enforces at construction time:
//   - Every session corresponds to exactly one profile.
//   - Every tool advertised by any session is reachable via
//     sessionFor(name).
//   - No two profiles advertise a tool with the same name (conflicts
//     are fatal at Handle() time so mis-configured deployments surface
//     loud errors instead of routing tool calls non-deterministically).
type sessionRegistry struct {
	// entries is in activation order, used by CloseAll and debug logs.
	entries []sessionEntry

	// byTool maps tool name -> servicing session. Sealed after build().
	byTool map[string]*mcp.ClientSession

	// dispatchByTool maps the model-facing tool name -> underlying MCP
	// tool name. Most tools keep the same name on both sides; router-owned
	// web-search tools are intentionally aliased so they do not collide
	// with generic client tool names such as "search" or "fetch".
	dispatchByTool map[string]string

	// owned is the union of router-owned tool names across all sessions.
	owned map[string]struct{}

	// tools is the union of advertised tools the upstream model sees.
	tools []*mcp.Tool
}

type sessionEntry struct {
	profile toolprofile.Profile
	session *mcp.ClientSession
	tools   []*mcp.Tool
}

// buildSessionRegistry opens one MCP session per profile and returns a
// sealed registry. On any error it closes every session it already
// opened so callers never have to worry about partial leaks. The
// dial argument is injected so tests can drive the registry without
// standing up real MCP servers; production callers pass
// connectOneToolSession which is the real attested-dial path.
func buildSessionRegistry(
	ctx context.Context,
	profiles []toolprofile.Profile,
	dial func(context.Context, toolprofile.Profile) (*mcp.ClientSession, error),
) (*sessionRegistry, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no tool profiles activated for this request")
	}

	r := &sessionRegistry{
		byTool:         make(map[string]*mcp.ClientSession),
		dispatchByTool: make(map[string]string),
		owned:          make(map[string]struct{}),
	}

	for _, profile := range profiles {
		session, err := dial(ctx, profile)
		if err != nil {
			r.CloseAll()
			return nil, fmt.Errorf("profile %q: %w", profile.Name, err)
		}
		toolsResult, err := session.ListTools(ctx, nil)
		if err != nil {
			session.Close()
			r.CloseAll()
			return nil, fmt.Errorf("profile %q list_tools: %w", profile.Name, err)
		}

		for _, tool := range toolsResult.Tools {
			outwardName := outwardRouterToolName(profile.Name, tool.Name)
			if existing, clash := r.byTool[outwardName]; clash {
				// We deliberately fail LOUD on overlapping tool names
				// rather than silently routing to whichever session
				// registered last. Namespacing would mask a genuine
				// misconfiguration and change the tool names the
				// upstream model sees, which is a contract break.
				_ = existing
				session.Close()
				r.CloseAll()
				return nil, fmt.Errorf(
					"tool name %q advertised by multiple profiles; rename or disable one",
					outwardName,
				)
			}
			r.byTool[outwardName] = session
			r.dispatchByTool[outwardName] = tool.Name
			r.owned[outwardName] = struct{}{}
		}

		r.entries = append(r.entries, sessionEntry{
			profile: profile,
			session: session,
			tools:   toolsResult.Tools,
		})
		for _, tool := range toolsResult.Tools {
			r.tools = append(r.tools, outwardRouterTool(profile.Name, tool))
		}
	}

	return r, nil
}

// sessionFor returns the MCP session that advertised the given tool
// name, or (nil, false) if no active profile owns it. The false
// branch is treated as a router programming error by callers: tools
// reach executeTool only after splitToolCalls has already classified
// them as router-owned against the same owned set this registry
// built.
func (r *sessionRegistry) sessionFor(toolName string) (*mcp.ClientSession, bool) {
	if r == nil {
		return nil, false
	}
	s, ok := r.byTool[toolName]
	return s, ok
}

func (r *sessionRegistry) dispatchName(toolName string) string {
	if r == nil {
		return dispatchRouterToolName(toolName)
	}
	if name, ok := r.dispatchByTool[toolName]; ok {
		return name
	}
	return dispatchRouterToolName(toolName)
}

// ownedTools returns the union of router-owned tool names across every
// active session. The returned map MUST NOT be mutated by callers; it
// backs the classification set splitToolCalls reads on every
// upstream iteration.
func (r *sessionRegistry) ownedTools() map[string]struct{} {
	if r == nil {
		return nil
	}
	return r.owned
}

// allTools returns the union of mcp.Tool records the registry holds.
// Upstream request builders pass this to chatTools / responseTools
// so every profile's tools land in the single tools array the model
// sees.
func (r *sessionRegistry) allTools() []*mcp.Tool {
	if r == nil {
		return nil
	}
	return r.tools
}

// profileNames returns the names of the profiles the registry was
// built from, in activation order. Used for debug logs so an
// operator can tell which servers a given request is running
// against.
func (r *sessionRegistry) profileNames() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		names = append(names, e.profile.Name)
	}
	return names
}

// endpointSummary returns a human-readable summary of the profiles the
// registry was built from, e.g. ["web_search (websearch)"].
// Used by devLog to display which MCP servers the request is running against.
func (r *sessionRegistry) endpointSummary() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.profile.Name+" ("+e.profile.ToolServerModel+")")
	}
	return out
}

// CloseAll closes every session the registry opened. Safe to call
// multiple times: each session's Close is idempotent in the MCP
// client, and the entries slice is walked once. CloseAll does not
// mutate the routing table because Handle() already returned to the
// caller by the time the deferred Close fires, so stale routing
// lookups are impossible.
func (r *sessionRegistry) CloseAll() {
	if r == nil {
		return
	}
	for _, e := range r.entries {
		if e.session != nil {
			_ = e.session.Close()
		}
	}
}
