package mcp

import (
	"strings"
	"testing"
)

// typedSegments is one user segment holding one block of the given type.
func typedSegments(blockType string) []map[string]any {
	return []map[string]any{{"role": "user", "content": []map[string]any{{"type": blockType, "text": "evidence"}}}}
}

// A block type the loop does not know is refused as a tool error on every
// tool that takes segments, against the real server: read as trusted text, a
// misspelled "untrusted-block" would run the caller's evidence as
// instructions. A block with no type is still trusted text.
func TestSegments_UnknownBlockTypeIsAToolError(t *testing.T) {
	for name, args := range map[string]func(blockType string) (tool string, args map[string]any){
		"spawn_run": func(bt string) (string, map[string]any) {
			return "spawn_run", map[string]any{"agent": "agent", "segments": typedSegments(bt)}
		},
		"spawn_runs": func(bt string) (string, map[string]any) {
			return "spawn_runs", map[string]any{"spawns": []map[string]any{
				{"agent": "agent", "segments": typedSegments("trusted-text")},
				{"agent": "agent", "segments": typedSegments(bt)},
			}}
		},
		"configured_run create": func(bt string) (string, map[string]any) {
			return "configured_run", map[string]any{"op": "create", "agent": "agent", "user_id": "u1", "segments": typedSegments(bt)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, prov, _ := configuredMCP(t)
			tool, bad := args("untrusted")
			res := callTool(t, srv, tool, bad)
			if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, `unknown type "untrusted"`) {
				t.Errorf("a block of type \"untrusted\" was not refused as a tool error: %+v", res)
			}
			if prov.last != nil {
				t.Error("a run started despite the refusal")
			}
			for _, good := range []string{"", "untrusted-block"} {
				tool, ok := args(good)
				if res := callTool(t, srv, tool, ok); res.IsError {
					t.Errorf("a block of type %q was refused: %+v", good, res.Content)
				}
			}
		})
	}
}
