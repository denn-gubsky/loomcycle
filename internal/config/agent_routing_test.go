package config

import (
	"strings"
	"testing"
	"time"
)

func TestAgentRouting_SwitchAfterDefaultsTo20s(t *testing.T) {
	var none *AgentRouting
	for _, r := range []*AgentRouting{none, {QueueAware: true}, {QueueAware: true, SwitchAfterMS: 0}} {
		if got := r.SwitchAfter(); got != 20*time.Second {
			t.Errorf("%+v: SwitchAfter = %s, want 20s", r, got)
		}
	}
	if got := (&AgentRouting{QueueAware: true, SwitchAfterMS: 5000}).SwitchAfter(); got != 5*time.Second {
		t.Errorf("switch_after_ms 5000: %s, want 5s", got)
	}
}

func TestValidateAgentRouting(t *testing.T) {
	cases := []struct {
		name            string
		r               *AgentRouting
		provider, model string
		want            string // "" = accepted
	}{
		{"none", nil, "anthropic", "m", ""},
		{"tier-routed", &AgentRouting{QueueAware: true, SwitchAfterMS: 1000}, "", "", ""},
		{"pinned without queue_aware", &AgentRouting{SwitchAfterMS: 1000}, "anthropic", "m", ""},
		{"pinned provider", &AgentRouting{QueueAware: true}, "anthropic", "", "a provider/model pin has only one"},
		{"pinned model", &AgentRouting{QueueAware: true}, "", "local-medium", "a provider/model pin has only one"},
		{"negative switch_after_ms", &AgentRouting{QueueAware: true, SwitchAfterMS: -1}, "", "", "switch_after_ms must be >= 0"},
		{"switch_after_ms of a day", &AgentRouting{QueueAware: true, SwitchAfterMS: 24 * 60 * 60 * 1000}, "", "", ""},
		{"switch_after_ms past a day", &AgentRouting{QueueAware: true, SwitchAfterMS: 24*60*60*1000 + 1}, "", "", "switch_after_ms must be <= 86400000"},
		// Multiplied by time.Millisecond, 2^54 wraps to a negative duration,
		// under which every candidate's wait counts as over.
		{"switch_after_ms that overflows a duration", &AgentRouting{QueueAware: true, SwitchAfterMS: 1 << 54}, "", "", "switch_after_ms must be <= 86400000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateAgentRouting(c.r, c.provider, c.model)
			if c.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// Operator yaml is held to the same rule, naming the agent; a tier-routed
// agent's block loads as written.
func TestLoad_AgentRoutingIsValidatedAndRead(t *testing.T) {
	base := `
defaults: { provider: anthropic, model: x }
tiers:
  middle: [{ provider: anthropic, model: x }]
agents:
`
	cfg, err := LoadLayers(Layer{Name: "op", Data: []byte(base + `
  busy-aware:
    tier: middle
    routing: { queue_aware: true, switch_after_ms: 15000 }
`)})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := cfg.Agents["busy-aware"].Routing
	if r == nil || !r.QueueAware || r.SwitchAfterMS != 15000 {
		t.Fatalf("routing = %+v, want queue_aware with 15000", r)
	}

	_, err = LoadLayers(Layer{Name: "op", Data: []byte(base + `
  pinned:
    provider: anthropic
    model: x
    routing: { queue_aware: true }
`)})
	if err == nil || !strings.Contains(err.Error(), `agent "pinned"`) || !strings.Contains(err.Error(), "routing.queue_aware") {
		t.Fatalf("err = %v, want the pinned agent refused naming routing.queue_aware", err)
	}
}
