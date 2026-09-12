package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// asOperator marks the fixture's ctx as an operator's own off-run call — what
// substrateAdminCtx and the MCP operator plane do in production. Derived from
// the fixture's ctx so it keeps the def-authoring policy; only the AUTHORITY
// differs, which is the axis under test.
func asOperator(ctx context.Context) context.Context {
	return tools.WithSubstrateOperator(ctx)
}

// asAgent is the fixture's ctx unchanged — a run authoring a def at runtime,
// with the same policy and a plausible agent id.
func asAgent(ctx context.Context, agentID string) context.Context {
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: agentID})
}

// TestAgentDef_OperatorAuthoredRoundTrips is the core claim: who wrote a def is
// recorded, and it comes from the CTX rather than from anything the caller can
// put in the body or the identity.
func TestAgentDef_OperatorAuthoredRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		as   func(context.Context) context.Context
		want bool
	}{
		{"an operator's own call", asOperator, true},
		{"an agent at runtime", func(c context.Context) context.Context { return asAgent(c, "a_some_agent") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, base, cleanup := agentDefFixture(t)
			defer cleanup()
			ctx := tc.as(base)

			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"rev","overlay":{"tier":"middle"}}`))
			if res.IsError {
				t.Fatalf("create: %s", res.Text)
			}
			defID, _ := decodeResult(t, res.Text)["def_id"].(string)

			row, err := tool.Store.AgentDefGet(ctx, defID)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if row.OperatorAuthored != tc.want {
				t.Errorf("operator_authored = %v, want %v", row.OperatorAuthored, tc.want)
			}
		})
	}
}

// TestAgentDef_AuthorshipCannotBeClaimedByTheBody: the flag is stamped from the
// ctx, so a def that names itself operator-authored in its overlay — or names
// an operator-looking agent as its author — gains nothing.
func TestAgentDef_AuthorshipCannotBeClaimedByTheBody(t *testing.T) {
	tool, base, cleanup := agentDefFixture(t)
	defer cleanup()
	// An AGENT's ctx, with an operator-LOOKING agent id and an overlay that
	// tries to assert authorship directly.
	ctx := asAgent(base, "a_operator")

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"create","name":"sneaky","overlay":{"tier":"middle","operator_authored":true}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	defID, _ := decodeResult(t, res.Text)["def_id"].(string)
	row, _ := tool.Store.AgentDefGet(ctx, defID)
	if row.OperatorAuthored {
		t.Error("a def claimed operator authorship from its own body or agent id — " +
			"authority must come from the ctx alone")
	}
}

// TestAgentDef_ForkDoesNotInheritAuthorship: a fork may change the body, so
// inheriting the parent's flag would launder an operator-authored def into an
// agent-authored one that still carried operator authority.
func TestAgentDef_ForkDoesNotInheritAuthorship(t *testing.T) {
	tool, base, cleanup := agentDefFixture(t)
	defer cleanup()

	opCtx := asOperator(base)
	res, _ := tool.Execute(opCtx, json.RawMessage(`{"op":"create","name":"base","overlay":{"tier":"middle"}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	parentID, _ := decodeResult(t, res.Text)["def_id"].(string)
	parent, _ := tool.Store.AgentDefGet(opCtx, parentID)
	if !parent.OperatorAuthored {
		t.Fatal("the operator-authored parent was not stamped")
	}

	// An AGENT forks it.
	agentCtx := asAgent(base, "a_agent")
	res, _ = tool.Execute(agentCtx, json.RawMessage(`{"op":"fork","name":"base","overlay":{"tier":"high"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forkID, _ := decodeResult(t, res.Text)["def_id"].(string)
	fork, _ := tool.Store.AgentDefGet(agentCtx, forkID)
	if fork.OperatorAuthored {
		t.Error("an agent's fork inherited the parent's operator authorship — " +
			"a fork may change the body, so authority must be re-earned")
	}

	// And the operator forking their own def keeps it.
	res, _ = tool.Execute(opCtx, json.RawMessage(`{"op":"fork","name":"base","overlay":{"tier":"low"}}`))
	opForkID, _ := decodeResult(t, res.Text)["def_id"].(string)
	opFork, _ := tool.Store.AgentDefGet(opCtx, opForkID)
	if !opFork.OperatorAuthored {
		t.Error("an operator's own fork lost its authorship")
	}
}
