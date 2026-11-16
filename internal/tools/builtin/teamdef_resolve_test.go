package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GS P0 — a team NAME resolves to the caller tenant's active version, else
// the shared ("") tier's, on every by-name read. Before it, a shared team was
// listed and forkable from a tenant (and runnable by def_id) but "not found"
// when run, verified or drawn by name.

// sharedTeamFixture creates team "shared-team" in the shared tier and returns
// the tool, its def_id and a caller in tenant acme.
func sharedTeamFixture(t *testing.T) (*TeamDef, string, context.Context) {
	t.Helper()
	tool, shared, done := teamDefFixture(t)
	t.Cleanup(done)
	created := createTeam(t, tool, shared, "shared-team", validTeamGraph)
	defID, _ := created["def_id"].(string)
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })
	acme := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_acme", TenantID: "acme"})
	return tool, defID, acme
}

func TestTeamDefRun_ByNameReachesASharedTeamFromATenant(t *testing.T) {
	tool, sharedID, acme := sharedTeamFixture(t)
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"run","name":"shared-team","input":"x"}`))
	if res.IsError {
		t.Fatalf("run of a shared team by name from tenant acme: %s", res.Text)
	}
	rec.mu.Lock()
	spec := rec.spec
	rec.mu.Unlock()
	if spec.DefID != sharedID || spec.ResolvedBy != "name" {
		t.Errorf("walk opened with def %q resolved by %q, want the shared version %q by name", spec.DefID, spec.ResolvedBy, sharedID)
	}
}

func TestTeamDefRun_ATenantsOwnVersionShadowsTheSharedOne(t *testing.T) {
	tool, sharedID, acme := sharedTeamFixture(t)
	forked, _ := tool.Execute(acme, json.RawMessage(`{"op":"fork","name":"shared-team","promote":true,"overlay":{"max_iterations":3}}`))
	if forked.IsError {
		t.Fatalf("acme's fork of the shared team: %s", forked.Text)
	}
	ownID, _ := decodeResult(t, forked.Text)["def_id"].(string)
	if ownID == "" || ownID == sharedID {
		t.Fatalf("fork def_id = %q (shared %q)", ownID, sharedID)
	}
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	if res, _ := tool.Execute(acme, json.RawMessage(`{"op":"run","name":"shared-team","input":"x"}`)); res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	rec.mu.Lock()
	got := rec.spec.DefID
	rec.mu.Unlock()
	if got != ownID {
		t.Errorf("walk ran def %q, want acme's own version %q", got, ownID)
	}

	// Retiring its own version takes the name out of service for acme; it does
	// not fall through to the shared version.
	if res, _ := tool.Execute(acme, json.RawMessage(`{"op":"retire","def_id":"`+ownID+`","retired":true}`)); res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"run","name":"shared-team","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "retired") {
		t.Errorf("run after acme retired its own version = %s, want refused as retired", res.Text)
	}
}

func TestTeamDefVerifyAndDiagram_ByNameReachASharedTeamFromATenant(t *testing.T) {
	tool, sharedID, acme := sharedTeamFixture(t)

	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"verify","name":"shared-team"}`))
	if res.IsError {
		t.Fatalf("verify: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["deployed"] != true || out["current_def_id"] != sharedID || out["shared"] != true {
		t.Errorf("verify = %s, want the shared version deployed and marked shared", res.Text)
	}

	res, _ = tool.Execute(acme, json.RawMessage(`{"op":"verify","name":"shared-team","overlay":{"max_iterations":4}}`))
	if res.IsError {
		t.Fatalf("verify with an overlay: %s", res.Text)
	}
	if out := decodeResult(t, res.Text); out["deployed"] != true || out["current_def_id"] != sharedID {
		t.Errorf("verify with an overlay = %s, want the shared version as the deployed one", res.Text)
	}

	if res, _ := tool.Execute(acme, json.RawMessage(`{"op":"render_diagram","name":"shared-team"}`)); res.IsError {
		t.Errorf("render_diagram of a shared team by name from tenant acme: %s", res.Text)
	}
}

func TestTeamDefRun_ANameInNeitherTierIsNotFound(t *testing.T) {
	tool, _, acme := sharedTeamFixture(t)
	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"run","name":"nobody","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "team not found") {
		t.Errorf("run of an unknown name = %s, want team not found", res.Text)
	}
	res, _ = tool.Execute(acme, json.RawMessage(`{"op":"verify","name":"nobody"}`))
	if res.IsError || decodeResult(t, res.Text)["deployed"] != false {
		t.Errorf("verify of an unknown name = %s, want deployed false", res.Text)
	}
}

// Another tenant's own team is still not reachable by name: the fallback is to
// the shared tier only.
func TestTeamDefRun_ByNameNeverReachesAnotherTenantsTeam(t *testing.T) {
	tool, _, acme := sharedTeamFixture(t)
	globex := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_globex", TenantID: "globex"})
	createTeam(t, tool, globex, "globex-team", validTeamGraph)
	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"run","name":"globex-team","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "team not found") {
		t.Errorf("acme running globex's team by name = %s, want team not found", res.Text)
	}
}

// The run verb is asked of a shared team as of a tenant's own.
func TestTeamDefRun_AnACLDenyOnASharedTeamRefusesItsRunByName(t *testing.T) {
	tool, _, acme := sharedTeamFixture(t)
	run := runJudgedBy(acme, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"run"}, Resources: []string{"team:shared-team"}}))
	res, _ := tool.Execute(run, json.RawMessage(`{"op":"run","name":"shared-team","input":"x"}`))
	if aclCode(res) != "acl_denied" || !strings.Contains(res.Text, "team:shared-team") {
		t.Errorf("run under a deny = %s, want acl_denied naming team:shared-team", res.Text)
	}
}

// A shared team that declares agents of its own runs only for the shared tier;
// a tenant is told to fork it into its own tenant, which works.
func TestTeamDefRun_ASharedTeamWithItsOwnAgentsTellsATenantToForkIt(t *testing.T) {
	tool, shared := localTeamFixture(t)
	createTeam(t, tool, shared, "sdlc", localTeam(`{"tier":"middle"}`))
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })
	acme := localAuthorCtx("acme", []string{"any"}, []string{"Read", "Grep", "Agent"})
	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"run","name":"sdlc","input":"x"}`))
	wantRefused(t, res, "a tenant running a shared team with its own agents", "shared team", "op=fork")
	if res := teamOp(t, tool, acme, "fork", "sdlc", `{"max_iterations":3}`); res.IsError {
		t.Fatalf("acme forking the shared team: %s", res.Text)
	}
}
