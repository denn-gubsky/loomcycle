package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A team's own agent holds the three one-way locks a stored agent holds —
// allow_overrides: false, retention: none and a read tool mode — across the
// team's versions: a fork, a create over the name or a promote of an older
// version may not drop one, and only the authority that may lift it for an
// AgentDef may lift it here.

// lockedReviewers are reviewer bodies holding each lock, with the same body
// without it.
var lockedReviewers = []struct {
	lock, locked, open, mention string
}{
	{"allow_overrides", `{"tier":"middle","allow_overrides":false}`, `{"tier":"middle"}`, "allow_overrides"},
	{"retention none", `{"tier":"middle","retention":"none"}`, `{"tier":"middle"}`, "no-trace"},
	{"read tool mode", `{"tier":"middle","tools":["Document"],"tool_modes":{"Document":"read"}}`, `{"tier":"middle","tools":["Document"]}`, "read mode"},
}

// localLockFixture is the team-local fixture with an author holding Document.
func localLockFixture(t *testing.T) (*TeamDef, context.Context) {
	t.Helper()
	tool, _ := localTeamFixture(t)
	tool.Agents.Audit = &noTraceAuditSink{}
	return tool, localAuthorCtx("", []string{"any"}, []string{"Read", "Grep", "Agent", "Document"})
}

func localAgentsOverlay(body string) string {
	return `{"local":{"agents":{"reviewer":` + body + `}}}`
}

// promotedFork forks team sdlc with overlay and makes the fork active.
func promotedFork(t *testing.T, tool *TeamDef, ctx context.Context, overlay string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"sdlc","promote":true,"overlay":`+overlay+`}`))
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	return res
}

func teamDefIDOf(t *testing.T, res tools.Result) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s", res.Text)
	}
	var out struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.DefID == "" {
		t.Fatalf("result %s: %v", res.Text, err)
	}
	return out.DefID
}

func TestTeamDefFork_LocalAgentKeepsItsLocks(t *testing.T) {
	for _, tc := range lockedReviewers {
		t.Run(tc.lock, func(t *testing.T) {
			tool, ctx := localLockFixture(t)
			v1 := teamDefIDOf(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(tc.open)))
			teamDefIDOf(t, promotedFork(t, tool, ctx, localAgentsOverlay(tc.locked)))

			wantRefused(t, teamOp(t, tool, ctx, "fork", "sdlc", localAgentsOverlay(tc.open)), "fork dropping the lock", tc.mention)
			wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(tc.open)), "create over the team without the lock", tc.mention)
			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+v1+`"}`))
			wantRefused(t, res, "promote of the version before the lock", tc.mention)
			res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"sdlc","parent_def_id":"`+v1+`","promote":true,"overlay":{"max_iterations":3}}`))
			wantRefused(t, res, "fork of the version before the lock", tc.mention)
			// In a run, too.
			wantRefused(t, teamOp(t, tool, tools.WithRunID(ctx, "r_team"), "fork", "sdlc", localAgentsOverlay(tc.open)), "a run's fork dropping the lock", tc.mention)

			// Keeping the lock is fine.
			if res := teamOp(t, tool, ctx, "fork", "sdlc", `{"max_iterations":3}`); res.IsError {
				t.Fatalf("fork keeping the lock: %s", res.Text)
			}
		})
	}
}

// Dropping a team's own agent is not a way round its locks: the newest
// version that declared it still holds them when it is declared again.
func TestTeamDefFork_LocalAgentReAddedAfterADropKeepsItsLocks(t *testing.T) {
	for _, tc := range lockedReviewers {
		t.Run(tc.lock, func(t *testing.T) {
			tool, ctx := localLockFixture(t)
			teamDefIDOf(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(tc.locked)))
			dropped := `{"local":{"agents":{"judge":{"tier":"low"}}},
			  "states":[{"state":"review","handler":{"kind":"agent","agent":"./judge"}},{"state":"done","handler":{"kind":"terminal"}}]}`
			teamDefIDOf(t, promotedFork(t, tool, ctx, dropped))
			readded := `{"local":{"agents":{"reviewer":` + tc.open + `}},
			  "states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},{"state":"done","handler":{"kind":"terminal"}}]}`
			wantRefused(t, promotedFork(t, tool, ctx, readded), "re-adding the agent without the lock", tc.mention)
		})
	}
}

// A tenant's first version of a shared team holds the shared team's locks,
// as a tenant's first version of a shared agent does.
func TestTeamDefFork_ATenantVersionOfASharedTeamKeepsItsLocalAgentsLocks(t *testing.T) {
	for _, tc := range lockedReviewers {
		t.Run(tc.lock, func(t *testing.T) {
			tool, ctx := localLockFixture(t)
			v1 := teamDefIDOf(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(tc.open)))
			teamDefIDOf(t, promotedFork(t, tool, ctx, localAgentsOverlay(tc.locked)))
			acme := localAuthorCtx("acme", []string{"any"}, []string{"Read", "Grep", "Agent", "Document"})
			res, _ := tool.Execute(acme, json.RawMessage(`{"op":"fork","name":"sdlc","parent_def_id":"`+v1+`","promote":true,"overlay":{"max_iterations":3}}`))
			wantRefused(t, res, "acme's fork of the shared version before the lock", tc.mention)
			if res := teamOp(t, tool, acme, "fork", "sdlc", `{"max_iterations":3}`); res.IsError {
				t.Fatalf("acme's fork of the shared active version: %s", res.Text)
			}
		})
	}
}

// The authority that may lift each lock on an AgentDef may lift it on a
// team's own agent: the operator plane for allow_overrides and a read mode,
// a substrate:tenant principal off-run for retention none (audited).
func TestTeamDefFork_TheOperatorMayLiftALocalAgentsLocks(t *testing.T) {
	for _, tc := range lockedReviewers {
		t.Run(tc.lock, func(t *testing.T) {
			tool, ctx := localLockFixture(t)
			teamDefIDOf(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(tc.locked)))
			op := operatorPlane(auth.WithPrincipal(ctx, auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeTenant}}))
			if res := promotedFork(t, tool, op, localAgentsOverlay(tc.open)); res.IsError {
				t.Fatalf("the operator lifting the lock: %s", res.Text)
			}
		})
	}
	tool, ctx := localLockFixture(t)
	sink := tool.Agents.Audit.(*noTraceAuditSink)
	teamDefIDOf(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle","retention":"none"}`)))
	op := operatorPlane(auth.WithPrincipal(ctx, auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeTenant}}))
	if res := teamOp(t, tool, op, "fork", "sdlc", localAgentsOverlay(`{"tier":"middle"}`)); res.IsError {
		t.Fatalf("the operator dropping retention none: %s", res.Text)
	}
	if len(sink.evs) != 1 || sink.evs[0].TargetName != "sdlc/reviewer" || sink.evs[0].Action != "retention_none_removed" {
		t.Errorf("audit records = %+v, want one retention_none_removed naming sdlc/reviewer", sink.evs)
	}
}

// On a team's own no-trace agent, adding or changing a hook is the operator's,
// as on a stored one: a hook is sent what a run says.
func TestTeamDefFork_LocalNoTraceAgentKeepsItsHooks(t *testing.T) {
	tool, ctx := localLockFixture(t)
	const hookA = `"hooks":{"run_end":[{"name":"h","url":"https://app.example/a"}]}`
	const hookB = `"hooks":{"run_end":[{"name":"h","url":"https://collector.example/b"}]}`
	teamDefIDOf(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle","retention":"none",`+hookA+`}`)))
	wantRefused(t, promotedFork(t, tool, ctx, localAgentsOverlay(`{"tier":"middle","retention":"none",`+hookB+`}`)), "fork changing the hook", "no-trace")
	if res := promotedFork(t, tool, ctx, localAgentsOverlay(`{"tier":"middle","retention":"none","system_prompt":"v2",`+hookA+`}`)); res.IsError {
		t.Fatalf("fork keeping the hook: %s", res.Text)
	}
}
