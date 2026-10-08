package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
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

// offRun is a context prepared as the server's run-less path prepares one: it
// names who is charged and carries no run and no agent.
func offRun() context.Context {
	return tools.WithMeteredOffRunCall(context.Background(), tools.MeteredOffRunCallValue{TenantID: "acme", UserID: "alice"})
}

// TestDecision_ARefusalIsWordedForItsCaller — the tool's refusals reach an
// agent inside a run and a caller that has no run and no agent (the HTTP, gRPC
// and MCP surfaces). A refusal must not tell the second about "this agent" or
// "this run": each text is either true for both or picked by which caller the
// context says it is, and its code, category and retryability are the same
// for both.
func TestDecision_ARefusalIsWordedForItsCaller(t *testing.T) {
	withKey := func(o *decision.Options) { o.APIKey, o.KeyEnvName = "test-operator-key", "OLLAMA_API_KEY" }
	nothingLeft := &config.AgentDecision{Models: []string{"withdrawn"}} // a narrowing the operator's list no longer meets
	barred := func(ctx context.Context) context.Context { return providers.WithOperatorKeyAllowed(ctx, false) }

	for _, c := range []struct {
		name     string
		prepare  func(context.Context) context.Context
		input    string
		code     string
		category tools.ErrorCategory
		inRun    string // the text after the code, for an agent in a run
		offRun   string // and for a caller with no run
	}{
		{"a model outside the list", nil, `{"model":"nope",` + oneNoul + `}`,
			decision.CodeModelNotAllowed, tools.CategoryValidation,
			`model "nope" is not one of the decision models you may ask`,
			`model "nope" is not one of the decision models you may ask`},
		{"no model left to ask", func(ctx context.Context) context.Context { return tools.WithDecisionPolicy(ctx, nothingLeft) }, `{` + oneNoul + `}`,
			decision.CodeModelNotAllowed, tools.CategoryValidation,
			"none of the decision models you may ask is offered by this deployment",
			"none of the decision models you may ask is offered by this deployment"},
		{"barred from the operator's key", barred, `{` + oneNoul + `}`,
			DecisionCodeKeyRestricted, tools.CategoryPermission,
			"this run may not use the operator's provider key, and has none of its own for this provider",
			"you may not use the operator's provider key, and have none of your own for this provider"},
	} {
		for _, caller := range []struct {
			name string
			ctx  context.Context
			want string
		}{{"in a run", inRun(), c.inRun}, {"with no run", offRun(), c.offRun}} {
			t.Run(c.name+"/"+caller.name, func(t *testing.T) {
				d := newDecisionDouble(t)
				ctx := caller.ctx
				if c.prepare != nil {
					ctx = c.prepare(ctx)
				}
				res := decide(t, decisionTool(t, d, withKey), ctx, c.input)
				if want := "Decision: " + c.code + ": " + caller.want; res.Text != want {
					t.Errorf("text = %q, want %q", res.Text, want)
				}
				if res.Error == nil || res.Error.Category != c.category || res.Error.Retryable {
					t.Errorf("classification = %+v, want %s and not retryable", res.Error, c.category)
				}
				if d.calls() != 0 {
					t.Errorf("a refused call reached the provider %d times", d.calls())
				}
			})
		}
	}
}

// TestDecision_TheNoRunHintNamesBothWaysIn — the caller refused for having no
// run is neither an agent nor the decision API's (both are served), so telling
// it only to start a run hides the way in that needs none.
func TestDecision_TheNoRunHintNamesBothWaysIn(t *testing.T) {
	d := newDecisionDouble(t)
	res := decide(t, decisionTool(t, d), context.Background(), `{`+oneNoul+`}`)
	if DecisionFailureCode(res.Text) != DecisionCodeNoRun || res.Error == nil {
		t.Fatalf("result = %q %+v, want a classified no_run", res.Text, res.Error)
	}
	for _, way := range []string{"a run of an agent that holds the Decision tool", "POST /v1/_decide", "gRPC Decide", "MCP `decision` tool"} {
		if !strings.Contains(res.Error.Description, way) {
			t.Errorf("the hint %q does not name %q", res.Error.Description, way)
		}
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
