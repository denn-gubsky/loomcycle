package http

import (
	"reflect"
	"testing"
)

// TestReferencedDynamicMCPServers covers the allowed_tools → server-set parsing
// that scopes the F33 run-start handshake.
func TestReferencedDynamicMCPServers(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		want     map[string]bool
	}{
		{"wildcard", []string{"mcp__telegram-dyn__*"}, map[string]bool{"telegram-dyn": true}},
		{"exact-tool", []string{"mcp__gitea__send_message"}, map[string]bool{"gitea": true}},
		{"mixed-native", []string{"Bash", "mcp__gitea__send", "Context", "mcp__gitea__list"}, map[string]bool{"gitea": true}},
		{"two-servers", []string{"mcp__a__*", "mcp__b__x"}, map[string]bool{"a": true, "b": true}},
		{"native-only", []string{"Bash", "Context"}, nil},
		{"allow-all-star", []string{"*"}, nil},
		{"a2a-peer", []string{"a2a__peer__skill"}, nil},
		{"malformed-no-tool-seg", []string{"mcp__srv"}, nil},
		{"malformed-empty-server", []string{"mcp____tool"}, nil},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := referencedDynamicMCPServers(tc.patterns)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("referencedDynamicMCPServers(%v) = %v, want %v", tc.patterns, got, tc.want)
			}
		})
	}
}
