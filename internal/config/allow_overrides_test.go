package config

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"gopkg.in/yaml.v3"
)

// allow_overrides reaches config.AgentDef from each place an agent can be
// declared, and a layered config can set it without restating the agent.
func TestAllowOverrides_ReachesTheAgentDefFromEverySource(t *testing.T) {
	var fromYAML AgentDef
	if err := yaml.Unmarshal([]byte("model: m\nallow_overrides: false\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromYAML.AllowOverrides == nil || *fromYAML.AllowOverrides {
		t.Errorf("yaml: allow_overrides = %v, want false", fromYAML.AllowOverrides)
	}
	var unset AgentDef
	if err := yaml.Unmarshal([]byte("model: m\n"), &unset); err != nil || unset.AllowOverrides != nil {
		t.Errorf("yaml without the key: %v (err %v), want unset", unset.AllowOverrides, err)
	}

	locked := false
	if got := agentFromDiscovered(&agents.Agent{Name: "a", AllowOverrides: &locked}); got.AllowOverrides == nil || *got.AllowOverrides {
		t.Errorf("a markdown agent's lock was dropped: %v", got.AllowOverrides)
	}

	// A later config layer locks an agent an earlier one declared, and a
	// layer that says nothing leaves the lock where it was.
	merged := mergeAgentDef(AgentDef{Model: "m"}, AgentDef{AllowOverrides: &locked})
	if merged.AllowOverrides == nil || *merged.AllowOverrides {
		t.Errorf("a layer's lock was dropped by the merge: %v", merged.AllowOverrides)
	}
	if kept := mergeAgentDef(merged, AgentDef{Effort: "low"}); kept.AllowOverrides == nil || *kept.AllowOverrides {
		t.Errorf("a layer that did not mention the lock removed it: %v", kept.AllowOverrides)
	}
}

func TestRetention_ReachesTheAgentDefFromEverySourceAndRefusesAnUnknownValue(t *testing.T) {
	var fromYAML AgentDef
	if err := yaml.Unmarshal([]byte("model: m\nretention: none\n"), &fromYAML); err != nil || fromYAML.Retention != "none" {
		t.Errorf("yaml: retention = %q (err %v)", fromYAML.Retention, err)
	}
	if got := agentFromDiscovered(&agents.Agent{Name: "a", Retention: "none"}); got.Retention != "none" {
		t.Errorf("a markdown agent's mode was dropped: %q", got.Retention)
	}
	merged := mergeAgentDef(AgentDef{Model: "m"}, AgentDef{Retention: "none"})
	if merged.Retention != "none" {
		t.Errorf("a layer's mode was dropped by the merge: %q", merged.Retention)
	}
	if kept := mergeAgentDef(merged, AgentDef{Effort: "low"}); kept.Retention != "none" {
		t.Errorf("a layer that did not mention the mode removed it: %q", kept.Retention)
	}
}
