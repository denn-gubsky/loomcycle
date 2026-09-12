package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// TestTeamDef_OperatorAuthoredRoundTrips: who wrote a TEAM is recorded, and it
// comes from the CTX rather than from anything the caller supplies.
func TestTeamDef_OperatorAuthoredRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		as   func(context.Context) context.Context
		want bool
	}{
		{"an operator's own call", asOperator, true},
		{"an agent at runtime", func(c context.Context) context.Context { return asAgent(c, "a_some_agent") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, base, done := teamDefFixture(t)
			defer done()
			ctx := tc.as(base)

			out := createTeam(t, tool, ctx, "rev", validTeamGraph)
			defID, _ := out["def_id"].(string)

			row, err := tool.Store.TeamDefGet(ctx, defID)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if row.OperatorAuthored != tc.want {
				t.Errorf("operator_authored = %v, want %v", row.OperatorAuthored, tc.want)
			}
		})
	}
}

// TestTeamDef_AuthorshipCannotBeClaimedByTheBody: the flag gates what a team's
// own node prompts may do, so a team that could assert it would be asserting
// its own authority. Neither the definition nor a plausible author id moves it.
func TestTeamDef_AuthorshipCannotBeClaimedByTheBody(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	// An agent naming itself something operator-shaped, whose definition also
	// tries to declare the flag.
	ctx := asAgent(base, "operator")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"claimer","overlay":{
		"entry": "review",
		"operator_authored": true,
		"states": [
			{"state": "review", "handler": {"kind": "agent", "agent": "reviewer"}},
			{"state": "done", "handler": {"kind": "terminal"}}
		],
		"transitions": [{"from": "review", "to": "done", "on": "success"}]
	}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	defID, _ := decodeResult(t, res.Text)["def_id"].(string)

	row, err := tool.Store.TeamDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if row.OperatorAuthored {
		t.Error("a team claimed operator authorship in its own body and the runtime believed it")
	}
}

// TestTeamDef_ForkDoesNotInheritAuthorship: a fork may rewrite every node
// prompt, so inheriting the parent's flag would launder an operator-authored
// team into an agent-authored one that kept the authority.
func TestTeamDef_ForkDoesNotInheritAuthorship(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()

	opCtx := asOperator(base)
	parent := createTeam(t, tool, opCtx, "rev", validTeamGraph)
	parentID, _ := parent["def_id"].(string)
	if row, err := tool.Store.TeamDefGet(opCtx, parentID); err != nil || !row.OperatorAuthored {
		t.Fatalf("the parent should be operator-authored: row=%+v err=%v", row, err)
	}

	agentCtx := asAgent(base, "a_forker")
	res, _ := tool.Execute(agentCtx, json.RawMessage(
		`{"op":"fork","name":"rev","from_def_id":"`+parentID+`","overlay":`+validTeamGraph+`}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forkID, _ := decodeResult(t, res.Text)["def_id"].(string)

	row, err := tool.Store.TeamDefGet(agentCtx, forkID)
	if err != nil {
		t.Fatalf("read the fork: %v", err)
	}
	if row.OperatorAuthored {
		t.Error("an agent forked an operator-authored team and inherited its authority — " +
			"a fork may rewrite every node prompt, so the flag has to come from the FORKER")
	}
}

// TestTeamDef_AuthorshipIsNotContent: the flag is AUTHORITY, so it must stay
// out of content_sha256 — otherwise the same team body authored by an operator
// and by an agent would hash differently, and a verify-or-fork across
// deployments would report a drift that is not one.
func TestTeamDef_AuthorshipIsNotContent(t *testing.T) {
	// SEPARATE stores so both can use the SAME NAME. teamgraph.Sign hashes the
	// name alongside the definition, so two names would differ for a reason
	// that has nothing to do with authorship — the axis under test has to be
	// the only thing that varies.
	opTool, opBase, opDone := teamDefFixture(t)
	defer opDone()
	agTool, agBase, agDone := teamDefFixture(t)
	defer agDone()

	opOut := createTeam(t, opTool, asOperator(opBase), "same-body", validTeamGraph)
	agOut := createTeam(t, agTool, asAgent(agBase, "a_x"), "same-body", validTeamGraph)

	opSHA, _ := opOut["content_sha256"].(string)
	agSHA, _ := agOut["content_sha256"].(string)
	if opSHA == "" {
		t.Fatal("no content_sha256 on the create response")
	}
	if opSHA != agSHA {
		t.Errorf("authorship changed the content hash (%s vs %s) — a verify across deployments "+
			"would report a drift that is not one", opSHA, agSHA)
	}
}

// TestTeamDef_RunCarriesAuthorshipOntoEveryNodePrompt is the thread this whole
// change exists to complete: the flag has to reach the PROMPT, because that is
// where the widened families are expanded.
//
// Asserted on what the spawner RECEIVED. A flag that stops one hop short is
// invisible from the outside — the walk still runs, the nodes still spawn, and
// the bindings just quietly render nothing.
func TestTeamDef_RunCarriesAuthorshipOntoEveryNodePrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		as   func(context.Context) context.Context
		want bool
	}{
		{"an operator's team", asOperator, true},
		{"an agent's team", func(c context.Context) context.Context { return asAgent(c, "a_x") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, base, done := teamDefFixture(t)
			defer done()
			ctx := tc.as(base)

			var seen []teamrun.Prompt
			tool.Spawn = func(_ context.Context, agent string, p teamrun.Prompt, _ string) (string, error) {
				seen = append(seen, p)
				return agent + " done", nil
			}
			createTeam(t, tool, ctx, "walked", validTeamGraph)

			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"walked","input":"go"}`))
			if res.IsError {
				t.Fatalf("run: %s", res.Text)
			}
			if len(seen) == 0 {
				t.Fatal("the walk spawned nothing")
			}
			for i, p := range seen {
				if p.SystemAuthored != tc.want {
					t.Errorf("prompt[%d].SystemAuthored = %v, want %v — the node's system "+
						"prompt is the TEAM's text and is expanded under this flag",
						i, p.SystemAuthored, tc.want)
				}
			}
		})
	}
}
