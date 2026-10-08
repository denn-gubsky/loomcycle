package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestDecision_ACallTheServerPreparedIsAnswered — the refusal of a call with no
// run is lifted for exactly one kind of context: the one the server's run-less
// path prepared, which names who is charged. Nothing else about the context
// does it: a run identity alone (what every MCP or admin dispatch stamps) is
// still refused.
func TestDecision_ACallTheServerPreparedIsAnswered(t *testing.T) {
	d := newDecisionDouble(t)
	tool := decisionTool(t, d, func(o *decision.Options) { o.APIKey, o.KeyEnvName = "test-operator-key", "OLLAMA_API_KEY" })

	identityOnly := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "alice"})
	if res := decide(t, tool, identityOnly, `{`+oneNoul+`}`); !strings.HasPrefix(res.Text, "Decision: no_run: ") || d.calls() != 0 {
		t.Fatalf("a dispatch carrying only an identity: %q after %d calls, want no_run and none", res.Text, d.calls())
	}
	prepared := tools.WithMeteredOffRunCall(context.Background(), tools.MeteredOffRunCallValue{TenantID: "acme", UserID: "alice"})
	if res := decide(t, tool, prepared, `{`+oneNoul+`}`); res.IsError || d.calls() != 1 {
		t.Errorf("a prepared call: %q after %d calls, want an answer and one call", res.Text, d.calls())
	}
}

// TestDecisionFailureCode_ReadsTheToolsOwnPrefix — a transport maps a failure
// onto its status by this code, so it must be the code the tool wrote and
// nothing a question's name or a message could smuggle in.
func TestDecisionFailureCode_ReadsTheToolsOwnPrefix(t *testing.T) {
	d := newDecisionDouble(t)
	tool := decisionTool(t, d)
	res := decide(t, tool, inRun(), `{"state":{},"questions":{"q":{"type":"noul","instructions":"x"}},"model":"nope"}`)
	if got := DecisionFailureCode(res.Text); got != decision.CodeModelNotAllowed {
		t.Errorf("code of %q = %q, want %q", res.Text, got, decision.CodeModelNotAllowed)
	}
	if got := DecisionFailureCode((&Decision{}).mustFail(t)); got != DecisionCodeNotConfigured {
		t.Errorf("code = %q, want %q", got, DecisionCodeNotConfigured)
	}
	for _, text := range []string{"", "unknown field \"x\"", "Decision: no colon after the code", "Other: invalid_input: x"} {
		if got := DecisionFailureCode(text); got != "" {
			t.Errorf("code of %q = %q, want none", text, got)
		}
	}
}

func (d *Decision) mustFail(t *testing.T) string {
	t.Helper()
	res := decide(t, d, context.Background(), `{}`)
	if !res.IsError {
		t.Fatalf("an unconfigured tool answered: %q", res.Text)
	}
	return res.Text
}
