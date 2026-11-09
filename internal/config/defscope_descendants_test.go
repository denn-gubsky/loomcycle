package config

import (
	"strings"
	"testing"
)

// `descendants` in a *_def_scopes gate never walked the lineage it promised:
// it granted every name, as `any` does. It is refused at load (RFC GD D1),
// in each field that takes the vocabulary, with the replacement named.
func TestLoad_ADescendantsDefScopeIsRefused(t *testing.T) {
	for _, field := range []string{"agent_def_scopes", "schedule_def_scopes", "a2a_server_card_def_scopes", "a2a_agent_def_scopes"} {
		body := `
defaults:
  provider: anthropic
  model: claude-haiku-4-5
agents:
  orchestrator:
    system_prompt: x
    tools: [AgentDef, ScheduleDef, A2AServerCardDef, A2AAgentDef]
    agent_def_scopes: [any]
    schedule_def_scopes: [any]
    a2a_server_card_def_scopes: [any]
    a2a_agent_def_scopes: [any]
`
		body = strings.Replace(body, field+": [any]", field+": [self, descendants]", 1)
		_, err := loadYAMLString(t, body)
		if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "descendants is not supported") {
			t.Errorf("%s: descendants: %v, want a refusal naming the field and the replacement", field, err)
		}
	}
}
