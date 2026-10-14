package toolruntime

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBuildRouterPromptOnlyMentionsAvailableWebSearchTools(t *testing.T) {
	cases := []struct {
		name      string
		tools     map[string]struct{}
		wantFetch bool
	}{
		{"fetch available", map[string]struct{}{routerSearchToolName: {}, routerFetchToolName: {}}, true},
		{"fetch disabled", map[string]struct{}{routerSearchToolName: {}}, false},
	}
	for _, tc := range cases {
		for _, harmony := range []bool{false, true} {
			prompt := buildRouterPrompt(harmony, []string{WebSearch.Name}, tc.tools)
			if len(prompt.Messages) != 1 {
				t.Fatalf("%s: got %d prompt messages, want 1", tc.name, len(prompt.Messages))
			}
			text := prompt.Messages[0].Content.(*mcp.TextContent).Text
			if !strings.Contains(text, routerSearchToolName) {
				t.Errorf("%s (harmony=%v): prompt must mention %q", tc.name, harmony, routerSearchToolName)
			}
			if got := strings.Contains(text, routerFetchToolName); got != tc.wantFetch {
				t.Errorf("%s (harmony=%v): mentions %q = %v, want %v", tc.name, harmony, routerFetchToolName, got, tc.wantFetch)
			}
		}
	}
}
