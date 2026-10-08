package config

import (
	"reflect"
	"strings"
	"testing"
)

const decisionBlock = `
decision:
  default: decide
  models: [decide, decide-deep, nimble-lit]
  provider: ollama-local
`

func agentWith(block string) string {
	return "agents:\n  router:\n    system_prompt: route\n    tools: [Decision]\n" + block
}

// TestAgentDecision_LoadsAndNarrowsOnlyWithinTheOperatorsList — an agent's
// block loads when every name is one the operator lists, and fails load,
// naming the agent and the name, when one is not. The operator's list is the
// ceiling: an agent's yaml cannot reach past it.
func TestAgentDecision_LoadsAndNarrowsOnlyWithinTheOperatorsList(t *testing.T) {
	cfg, err := Load(writeCfg(t, decisionCfg(decisionBlock+agentWith("    decision: { default: decide-deep, models: [decide-deep, nimble-lit] }\n"))))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := &AgentDecision{Default: "decide-deep", Models: []string{"decide-deep", "nimble-lit"}}
	if got := cfg.Agents["router"].Decision; !reflect.DeepEqual(got, want) {
		t.Errorf("agent decision = %+v, want %+v", got, want)
	}

	for _, c := range []struct{ name, block, want string }{
		{"a model outside the list", "    decision: { models: [decide, clef] }\n", `decision.models: "clef" is not one of the decision models`},
		{"a default outside the list", "    decision: { default: plain }\n", `decision.default: "plain" is not one of the decision models`},
		{"a default outside its own models", "    decision: { default: decide, models: [decide-deep] }\n", `decision.default: "decide" is not one of decision.models`},
		{"a model listed twice", "    decision: { models: [decide, decide] }\n", `"decide" is listed twice`},
	} {
		_, err := Load(writeCfg(t, decisionCfg(decisionBlock+agentWith(c.block))))
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), `agent "router"`) {
			t.Errorf("%s: err = %v, want agent \"router\" and %q", c.name, err, c.want)
		}
	}

	// With no operator block there is nothing to narrow.
	_, err = Load(writeCfg(t, decisionCfg(agentWith("    decision: { models: [decide] }\n"))))
	if err == nil || !strings.Contains(err.Error(), "declares no decision models") {
		t.Errorf("no operator block: err = %v, want a refusal", err)
	}
}

// TestAgentDecision_PickResolvesTheCallsModel — the rule the Decision tool
// applies: the agent's list bounds what a call may name; a call naming none
// gets the agent's default, else the operator's when the agent may name it,
// else the first of the agent's list.
func TestAgentDecision_PickResolvesTheCallsModel(t *testing.T) {
	operator := []string{"decide", "decide-deep", "nimble-lit"}
	for _, c := range []struct {
		name        string
		d           *AgentDecision
		asked       string
		want        string
		wantAllowed []string
		wantOK      bool
	}{
		{"no narrowing, none asked: the operator's default", nil, "", "decide", operator, true},
		{"no narrowing, a listed name", nil, "nimble-lit", "nimble-lit", operator, true},
		{"no narrowing, an unlisted name", nil, "clef", "clef", operator, false},
		{"narrowed, none asked: the agent's default",
			&AgentDecision{Default: "nimble-lit", Models: []string{"decide-deep", "nimble-lit"}}, "", "nimble-lit", []string{"decide-deep", "nimble-lit"}, true},
		{"narrowed with no default, the operator's default is inside",
			&AgentDecision{Models: []string{"decide-deep", "decide"}}, "", "decide", []string{"decide-deep", "decide"}, true},
		{"narrowed with no default, the operator's default is outside: the first listed",
			&AgentDecision{Models: []string{"nimble-lit", "decide-deep"}}, "", "nimble-lit", []string{"nimble-lit", "decide-deep"}, true},
		{"narrowed, a name inside", &AgentDecision{Models: []string{"decide-deep"}}, "decide-deep", "decide-deep", []string{"decide-deep"}, true},
		{"narrowed, an operator's name outside the agent's list", &AgentDecision{Models: []string{"decide-deep"}}, "decide", "decide", []string{"decide-deep"}, false},
		{"a default only: the list stays the operator's", &AgentDecision{Default: "decide-deep"}, "", "decide-deep", operator, true},
		{"a stored name the operator dropped is not allowed",
			&AgentDecision{Default: "gone", Models: []string{"gone", "decide-deep"}}, "gone", "gone", []string{"decide-deep"}, false},
		{"a dropped default falls through to what is left",
			&AgentDecision{Default: "gone", Models: []string{"gone", "decide-deep"}}, "", "decide-deep", []string{"decide-deep"}, true},
		{"every listed name dropped: nothing to ask", &AgentDecision{Models: []string{"gone"}}, "", "", nil, false},
	} {
		got, allowed, ok := c.d.Pick(c.asked, "decide", operator)
		if got != c.want || ok != c.wantOK || !reflect.DeepEqual(allowed, c.wantAllowed) {
			t.Errorf("%s: Pick = %q, %v, %v; want %q, %v, %v", c.name, got, allowed, ok, c.want, c.wantAllowed, c.wantOK)
		}
	}
}

// TestAgentDecision_ListingTheToolWithNoBlockWarns — the tool is not offered
// on a deployment that declares no decision models, so load says which agent
// lists it for nothing; with a block there is nothing to say.
func TestAgentDecision_ListingTheToolWithNoBlockWarns(t *testing.T) {
	decisionWarnings := func(c *Config) []string {
		var out []string
		for _, w := range c.Warnings {
			if strings.Contains(w, "tools includes Decision") {
				out = append(out, w)
			}
		}
		return out
	}
	cfg, err := Load(writeCfg(t, decisionCfg(agentWith(""))))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := decisionWarnings(cfg); len(got) != 1 || !strings.Contains(got[0], `agent "router"`) || !strings.Contains(got[0], "not offered") {
		t.Errorf("warnings = %q, want one naming the agent", got)
	}
	cfg, err = Load(writeCfg(t, decisionCfg(decisionBlock+agentWith(""))))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := decisionWarnings(cfg); len(got) != 0 {
		t.Errorf("warnings with a block = %q, want none", got)
	}
}
