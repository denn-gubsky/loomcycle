package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSpawnRun_FreshRunWithoutSegmentsIsRefused is the parity fix for a silent
// failure that cost an afternoon of misattributed debugging.
//
// A fresh MCP spawn with no segments reached the model as a NULL user turn: it got
// the system prompt and nothing to act on, answered whatever that implied, and the
// run COMPLETED — so nothing surfaced as an error. A tool-less extractor replying
// "[]" to an absent transcript is indistinguishable from a model that found
// nothing, and the emptiness was only visible by reading the thinking trace.
//
// HTTP has refused this since F47 rather than dispatching an empty messages array,
// so the same caller error was a 400 on one transport and a silently-empty run on
// the other.
func TestSpawnRun_FreshRunWithoutSegmentsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want bool // true = must be refused
	}{
		{"fresh run, segments omitted", `{"agent":"qa"}`, true},
		{"fresh run, segments empty", `{"agent":"qa","segments":[]}`, true},
		{"fresh run WITH segments", `{"agent":"qa","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`, false},
		// A continuation may legitimately have nothing new to add — the schema says
		// so, and "resume this session" is a real call. The guard must not catch it.
		{"continuation, no segments", `{"session_id":"s_1"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := &handlerEnv{connector: &mockConnector{}}
			res, err := handleSpawnRun(context.Background(), env, json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("handleSpawnRun: %v", err)
			}
			refused := res != nil && res.IsError
			if refused != tc.want {
				t.Errorf("refused=%v want=%v; result=%+v", refused, tc.want, res)
			}
			if tc.want && refused {
				// The message must name the fix, not just the rule — an operator
				// hitting this needs the shape to pass, since the field is easy to
				// get wrong (there is no top-level `prompt` on this transport).
				txt := ""
				if len(res.Content) > 0 {
					txt = res.Content[0].Text
				}
				if !strings.Contains(txt, "segments") || !strings.Contains(txt, "trusted-text") {
					t.Errorf("refusal does not show the required shape: %q", txt)
				}
			}
		})
	}
}
