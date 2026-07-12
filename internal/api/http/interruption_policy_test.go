package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestInterruptionPolicyForAgent_ToolPresenceEnables pins the ergonomics change:
// listing the Interruption tool in an agent's `tools` allowlist enables the tool
// (no separate interruption.enabled flag needed) — the redundant second gate
// that caused live "not enabled" errors is gone. An explicit enabled:true still
// enables even without the tool; no tool + no flag = disabled.
func TestInterruptionPolicyForAgent_ToolPresenceEnables(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		def  config.AgentDef
		want bool
	}{
		{"tool present, no flag", config.AgentDef{Tools: []string{"Read", "Interruption"}}, true},
		{"wildcard tools", config.AgentDef{Tools: []string{"*"}}, true},
		{"no tool, no flag", config.AgentDef{Tools: []string{"Read"}}, false},
		{"no tool but explicit enabled", config.AgentDef{Tools: []string{"Read"}, Interruption: config.AgentInterruptionACL{Enabled: true}}, true},
		{"empty tools", config.AgentDef{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.interruptionPolicyForAgent(tc.def).Enabled; got != tc.want {
				t.Errorf("Enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
