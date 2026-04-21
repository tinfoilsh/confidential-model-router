package manager

import "testing"

// TestLocalMCPEndpointEnvVar pins the exact shape of the debug-bypass
// env var name derived from a model name. The router advertises this
// contract to operators setting up local tool-server development, so
// any change here is a user-visible API break: new MCP servers must
// be able to predict their override env var from their model name.
func TestLocalMCPEndpointEnvVar(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"websearch", "LOCAL_MCP_ENDPOINT_WEBSEARCH"},
		{"code-runner", "LOCAL_MCP_ENDPOINT_CODE_RUNNER"},
		{"file-search-v2", "LOCAL_MCP_ENDPOINT_FILE_SEARCH_V2"},
		{"upper-CASE", "LOCAL_MCP_ENDPOINT_UPPER_CASE"},
		{"with space", "LOCAL_MCP_ENDPOINT_WITH_SPACE"},
	}
	for _, tc := range cases {
		if got := localMCPEndpointEnvVar(tc.model); got != tc.want {
			t.Errorf("localMCPEndpointEnvVar(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}
