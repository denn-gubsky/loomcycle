package config

import (
	"strings"
	"testing"
)

func TestInertToolGrants_ReportsOnlyGatesThatStillGrantNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  AgentDef
		want []string // tool names, in order
	}{
		{
			name: "a def-authoring tool with no scope is inert",
			def:  AgentDef{Tools: []string{"AgentDef"}},
			want: []string{"AgentDef"},
		},
		{
			name: "Channel with neither side of the ACL is inert",
			def:  AgentDef{Tools: []string{"Channel"}},
			want: []string{"Channel"},
		},
		{
			// Half-granted is not inert: publish works.
			name: "Channel with publish only is NOT reported",
			def:  AgentDef{Tools: []string{"Channel"}, Channels: AgentChannelACL{Publish: []string{"x"}}},
			want: nil,
		},
		{
			// THE POINT OF THE FILTER. These now resolve to what the caller owns,
			// so an event for them would fire on nearly every run — which is how
			// a signal becomes noise and then gets ignored.
			name: "Memory, History and Evaluation are NOT inert any more",
			def:  AgentDef{Tools: []string{"Memory", "History", "Evaluation"}},
			want: nil,
		},
		{
			name: "a gate the operator set produces nothing",
			def:  AgentDef{Tools: []string{"AgentDef"}, AgentDefScopes: []string{"self"}},
			want: nil,
		},
		{
			name: "several at once, in a stable order",
			def:  AgentDef{Tools: []string{"VolumeDef", "AgentDef", "Channel"}},
			want: []string{"AgentDef", "VolumeDef", "Channel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := InertToolGrants(tc.def)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d grants %v, want %v", len(got), names(got), tc.want)
			}
			for i, w := range tc.want {
				if got[i].Tool != w {
					t.Errorf("grant %d = %q, want %q (order must be stable)", i, got[i].Tool, w)
				}
			}
		})
	}
}

// The message has to name the GATE, not just the tool. A reader told only
// "AgentDef is inert" still has to work out which yaml key governs it, and that
// is not guessable from the tool name — which is most of why this was hard to
// act on.
func TestInertToolGrants_TheMessageNamesTheGateAndTheFix(t *testing.T) {
	got := InertToolGrants(AgentDef{Tools: []string{"VolumeDef"}})
	if len(got) != 1 {
		t.Fatalf("got %v", names(got))
	}
	for _, want := range []string{"VolumeDef", "volume_def_scopes", "named:"} {
		if !strings.Contains(got[0].Message, want) {
			t.Errorf("message does not mention %q: %s", want, got[0].Message)
		}
	}
	if got[0].Gate != "volume_def_scopes" {
		t.Errorf("gate = %q", got[0].Gate)
	}
}

func names(g []InertToolGrant) []string {
	out := make([]string, 0, len(g))
	for _, x := range g {
		out = append(out, x.Tool)
	}
	return out
}
