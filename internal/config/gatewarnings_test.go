package config

import (
	"strings"
	"testing"
)

// The scope defaults made these warnings WRONG, and a warning that states the
// wrong consequence is worse than none: it sends an operator to fix what is not
// broken and teaches them to ignore the channel.
//
// An unset memory_scopes no longer denies — it resolves to the caller's own
// data. An unset evaluation_scopes resolves to submit_self. The def-authoring
// gates and Channel still grant nothing, and must still say so.
func TestAgentGateWarnings_SayWhatAnEmptyGateNowActuallyDoes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		def        AgentDef
		wantPhrase string
		wantAbsent string
	}{
		{
			name:       "Memory defaults rather than denies",
			def:        AgentDef{Tools: []string{"Memory"}},
			wantPhrase: "caller's own data",
			wantAbsent: "every Memory op will default-deny",
		},
		{
			name:       "Evaluation defaults to judging itself",
			def:        AgentDef{Tools: []string{"Evaluation"}},
			wantPhrase: "submit_self",
			wantAbsent: "every Evaluation op will default-deny",
		},
		{
			// These are the gates that DID stay deny, and the warning has to keep
			// saying so — reporting them the same way as the defaulted ones is
			// what would make the whole report useless.
			name:       "AgentDef still denies outright",
			def:        AgentDef{Tools: []string{"AgentDef"}},
			wantPhrase: "will default-deny",
		},
		{
			name:       "Channel still denies outright",
			def:        AgentDef{Tools: []string{"Channel"}},
			wantPhrase: "will default-deny",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(agentGateWarnings("a", tc.def), "\n")
			if !strings.Contains(got, tc.wantPhrase) {
				t.Errorf("warning does not mention %q:\n%s", tc.wantPhrase, got)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("warning still claims %q, which stopped being true when the gate "+
					"gained a default:\n%s", tc.wantAbsent, got)
			}
		})
	}
}

// A gate the operator SET produces no advice. Reporting someone's own decision
// back at them as a problem is how a check earns being ignored.
func TestAgentGateWarnings_SayNothingAboutAGateThatIsSet(t *testing.T) {
	got := agentGateWarnings("a", AgentDef{
		Tools:            []string{"Memory", "Evaluation", "AgentDef"},
		MemoryScopes:     []string{"agent"},
		EvaluationScopes: []string{"submit_self"},
		AgentDefScopes:   []string{"self"},
	})
	if len(got) != 0 {
		t.Errorf("a fully-gated agent produced advisories:\n%s", strings.Join(got, "\n"))
	}
}

// The core-blocks trap is the one case where the DEFAULT is the wrong scope:
// core blocks live in the AGENT scope and the default is the caller's own, so
// "it defaults" is not reassurance here.
func TestAgentGateWarnings_CoreBlocksStillWantTheAgentScopeNamed(t *testing.T) {
	got := strings.Join(agentGateWarnings("a", AgentDef{
		Tools:      []string{"Memory"},
		CoreBlocks: []CoreBlock{{Label: "task"}},
	}), "\n")
	if !strings.Contains(got, "memory_scopes: [agent]") && !strings.Contains(got, "NOT the agent scope") {
		t.Errorf("the core-blocks advisory does not say the default is the wrong scope for it:\n%s", got)
	}
}
