package manager

import "testing"

func TestBearerTokenMatchesShimParsing(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer tk_test", "tk_test"},
		{"bearer tk_test", "tk_test"},
		{"BEARER   tk_test  ", "tk_test"},
		{"Token tk_test", ""},
		{"Bearer", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := BearerToken(tt.header); got != tt.want {
			t.Errorf("BearerToken(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}
