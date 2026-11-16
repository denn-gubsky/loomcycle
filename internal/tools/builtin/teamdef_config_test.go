package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GS P2 — the yaml's teams written into the shared tier at boot.

// A second graph for "the yaml changed": the same team with a cap.
const cappedTeamGraph = `{"entry":"review","max_iterations":3,
  "states":[{"state":"review","handler":{"kind":"agent","agent":"reviewer"}},{"state":"done","handler":{"kind":"terminal"}}],
  "transitions":[{"from":"review","to":"done","on":"success"}]}`

func configTeams(pairs ...string) map[string]config.Team {
	out := map[string]config.Team{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = config.Team{Description: "from the yaml", Definition: json.RawMessage(pairs[i+1])}
	}
	return out
}

// operatorGrants stands in for the server's substrate admin ctx, which the
// real caller (Server.ReconcileConfigTeams) stamps: any agent name, every
// tool, every skill. api/http tests the reconcile through the real one.
func operatorGrants() context.Context {
	return tools.UpdateRunPolicy(context.Background(), func(rp *tools.RunPolicy) {
		rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}}
		rp.Skill = tools.SkillPolicyValue{Granted: true}
		rp.Tools = []string{"*"}
	})
}

func reconcile(t *testing.T, tool *TeamDef, teams map[string]config.Team) ConfigTeamsReport {
	t.Helper()
	rep, err := tool.ReconcileConfigTeams(operatorGrants(), teams)
	if err != nil {
		t.Fatalf("ReconcileConfigTeams: %v", err)
	}
	return rep
}

func sharedActive(t *testing.T, tool *TeamDef, name string) store.TeamDefRow {
	t.Helper()
	row, err := tool.Store.TeamDefGetActive(context.Background(), "", name)
	if err != nil {
		t.Fatalf("shared active %q: %v", name, err)
	}
	return row
}

func sharedVersions(t *testing.T, tool *TeamDef, name string) []store.TeamDefRow {
	t.Helper()
	rows, err := tool.Store.TeamDefListByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.TeamDefRow
	for _, r := range rows {
		if r.TenantID == "" {
			out = append(out, r)
		}
	}
	return out
}

func TestReconcileConfigTeams_FirstBootWritesAndActivatesARestartWritesNothing(t *testing.T) {
	tool, _, done := teamDefFixture(t)
	defer done()

	rep := reconcile(t, tool, configTeams("review", validTeamGraph))
	if !reflect.DeepEqual(rep.Written, []string{"review"}) {
		t.Fatalf("first boot report = %+v, want review written", rep)
	}
	row := sharedActive(t, tool, "review")
	if !row.BootstrappedFromStatic || !row.OperatorAuthored || row.Retired || row.Description != "from the yaml" {
		t.Errorf("written version = %+v, want config-written, operator-authored, live, with the yaml's description", row)
	}

	rep = reconcile(t, tool, configTeams("review", validTeamGraph))
	if !reflect.DeepEqual(rep.Unchanged, []string{"review"}) || len(rep.Written) != 0 {
		t.Errorf("second boot report = %+v, want review unchanged", rep)
	}
	if n := len(sharedVersions(t, tool, "review")); n != 1 {
		t.Errorf("versions after a restart = %d, want 1", n)
	}
}

func TestReconcileConfigTeams_AChangedTeamIsANewVersionOnTheLastOne(t *testing.T) {
	tool, _, done := teamDefFixture(t)
	defer done()
	reconcile(t, tool, configTeams("review", validTeamGraph))
	first := sharedActive(t, tool, "review")

	rep := reconcile(t, tool, configTeams("review", cappedTeamGraph))
	if !reflect.DeepEqual(rep.Written, []string{"review"}) {
		t.Fatalf("report = %+v, want review written", rep)
	}
	second := sharedActive(t, tool, "review")
	if second.DefID == first.DefID || second.ParentDefID != first.DefID || second.Version != 2 {
		t.Errorf("changed team = %s v%d parent %q, want a v2 on %s", second.DefID, second.Version, second.ParentDefID, first.DefID)
	}

	// A description is not in the content hash, but a changed one is the yaml's.
	teams := configTeams("review", cappedTeamGraph)
	teams["review"] = config.Team{Description: "reworded", Definition: teams["review"].Definition}
	reconcile(t, tool, teams)
	if got := sharedActive(t, tool, "review"); got.Description != "reworded" || got.Version != 3 {
		t.Errorf("after a description change: v%d %q, want v3 reworded", got.Version, got.Description)
	}
}

// RFC GS D5: a team that leaves the yaml is retired, not deleted, and a run of
// it is refused; put back unchanged, it is in service again.
func TestReconcileConfigTeams_ARemovedTeamIsRetiredAndComesBackWhenReAdded(t *testing.T) {
	tool, shared, done := teamDefFixture(t)
	defer done()
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })
	reconcile(t, tool, configTeams("review", validTeamGraph, "other", validTeamGraph))

	rep := reconcile(t, tool, configTeams("other", validTeamGraph))
	if !reflect.DeepEqual(rep.Retired, []string{"review"}) {
		t.Fatalf("report = %+v, want review retired", rep)
	}
	if row := sharedActive(t, tool, "review"); !row.Retired {
		t.Errorf("removed team = %+v, want retired", row)
	}
	if res, _ := tool.Execute(shared, json.RawMessage(`{"op":"run","name":"review","input":"x"}`)); !res.IsError || !strings.Contains(res.Text, "retired") {
		t.Errorf("run of a removed team = %s, want refused as retired", res.Text)
	}

	rep = reconcile(t, tool, configTeams("review", validTeamGraph, "other", validTeamGraph))
	if !reflect.DeepEqual(rep.Restored, []string{"review"}) {
		t.Fatalf("report = %+v, want review restored", rep)
	}
	if res, _ := tool.Execute(shared, json.RawMessage(`{"op":"run","name":"review","input":"x"}`)); res.IsError {
		t.Errorf("run of a restored team: %s", res.Text)
	}
	if n := len(sharedVersions(t, tool, "review")); n != 1 {
		t.Errorf("versions = %d, want the one version, restored", n)
	}
}

// RFC GS D3: a name an operator made over the API in the shared tier is not
// taken over, and is not retired when the yaml lacks it.
func TestReconcileConfigTeams_AnOperatorsOwnSharedTeamIsNeitherReplacedNorRetired(t *testing.T) {
	tool, shared, done := teamDefFixture(t)
	defer done()
	created := createTeam(t, tool, shared, "review", validTeamGraph)

	rep := reconcile(t, tool, configTeams("review", cappedTeamGraph))
	if len(rep.Conflicts) != 1 || rep.Conflicts[0].Name != "review" || len(rep.Written) != 0 {
		t.Fatalf("report = %+v, want review in conflict and nothing written", rep)
	}
	if row := sharedActive(t, tool, "review"); row.DefID != created["def_id"] || row.BootstrappedFromStatic {
		t.Errorf("active = %+v, want the operator's own version untouched", row)
	}

	rep = reconcile(t, tool, nil)
	if len(rep.Retired) != 0 || sharedActive(t, tool, "review").Retired {
		t.Errorf("report = %+v: an API-made team was retired for not being in the yaml", rep)
	}
}

// Only the shared tier is written: a tenant's own version of the name stays
// what the tenant made, and still shadows the configured one for the tenant.
func TestReconcileConfigTeams_ATenantsOwnVersionIsUntouched(t *testing.T) {
	tool, _, done := teamDefFixture(t)
	defer done()
	acme := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_acme", TenantID: "acme"})
	own := createTeam(t, tool, acme, "review", validTeamGraph)

	reconcile(t, tool, configTeams("review", cappedTeamGraph))
	reconcile(t, tool, nil)
	row, err := tool.Store.TeamDefGetActive(context.Background(), "acme", "review")
	if err != nil || row.DefID != own["def_id"] || row.Retired || row.Version != 1 {
		t.Errorf("acme's active = %+v (%v), want its own v1, live", row, err)
	}
}

// A team is held to the create path's gates: the yaml cannot write what the
// API would refuse. A local agent body with a key AgentDef does not take is
// caught here — config's own load check leaves local bodies' keys to it.
func TestReconcileConfigTeams_ATeamTheCreatePathRefusesIsNotWritten(t *testing.T) {
	tool, _ := localTeamFixture(t)
	rep := reconcile(t, tool, configTeams(
		"good", localTeam(`{"tier":"middle"}`),
		"bad", localTeam(`{"tier":"middle","toolz":["Read"]}`),
	))
	if !reflect.DeepEqual(rep.Written, []string{"good"}) || len(rep.Refused) != 1 || rep.Refused[0].Name != "bad" {
		t.Fatalf("report = %+v, want good written and bad refused", rep)
	}
	if !strings.Contains(rep.Refused[0].Detail, "toolz") {
		t.Errorf("refusal %q should name the key", rep.Refused[0].Detail)
	}
	if _, err := tool.Store.TeamDefGetActive(context.Background(), "", "bad"); !isNotFound(err) {
		t.Errorf("refused team was written: %v", err)
	}
}

// A team from the yaml and the same team made over the API are the same team:
// one content hash, one stored body. A reader is told which came from the
// configuration.
func TestReconcileConfigTeams_AYAMLTeamIsTheAPITeamMarkedSourceConfig(t *testing.T) {
	tool, shared, done := teamDefFixture(t)
	defer done()
	reconcile(t, tool, configTeams("from-yaml", validTeamGraph))
	viaAPI := createTeam(t, tool, shared, "from-api", validTeamGraph)

	yamlRow := sharedActive(t, tool, "from-yaml")
	apiRow := sharedActive(t, tool, "from-api")
	if string(yamlRow.Definition) != string(apiRow.Definition) {
		t.Errorf("stored bodies differ:\nyaml %s\napi  %s", yamlRow.Definition, apiRow.Definition)
	}
	signedAs := func(name string, body []byte) string {
		res, _ := tool.Execute(shared, json.RawMessage(`{"op":"verify","name":"`+name+`","overlay":`+string(body)+`}`))
		return decodeResult(t, res.Text)["content_sha256"].(string)
	}
	if signedAs("from-yaml", yamlRow.Definition) != yamlRow.ContentSHA256 {
		t.Errorf("config version's content_sha256 %s is not what the API signs the same body as", yamlRow.ContentSHA256)
	}

	res, _ := tool.Execute(shared, json.RawMessage(`{"op":"get","def_id":"`+yamlRow.DefID+`"}`))
	if got := decodeResult(t, res.Text)["source"]; got != "config" {
		t.Errorf("get of a config version: source = %v, want config", got)
	}
	res, _ = tool.Execute(shared, json.RawMessage(`{"op":"list","name":"from-yaml"}`))
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if got := versions[0].(map[string]any)["source"]; got != "config" {
		t.Errorf("list of a config team: source = %v, want config", got)
	}
	res, _ = tool.Execute(shared, json.RawMessage(`{"op":"get","def_id":"`+viaAPI["def_id"].(string)+`"}`))
	if _, ok := decodeResult(t, res.Text)["source"]; ok {
		t.Errorf("get of an API version carries source: %s", res.Text)
	}
}

// The promoter of a configured team is the posture a static schedule runs
// under: nothing restricted, no access-control owner.
func TestReconcileConfigTeams_AConfiguredTeamIsPromotedUnrestricted(t *testing.T) {
	tool, _, done := teamDefFixture(t)
	defer done()
	reconcile(t, tool, configTeams("review", validTeamGraph))
	entries, err := tool.Store.(interface {
		SnapshotReadTeamDefActive(context.Context) ([]store.TeamDefActiveEntry, error)
	}).SnapshotReadTeamDefActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name != "review" {
			continue
		}
		if e.Promoter == nil || e.Promoter.OperatorKeyRestricted || e.Promoter.Isolated || e.Promoter.ACLOwner != nil || e.PromotedByAgentID != configTeamAuthor {
			t.Errorf("promoter = %+v by %q, want unrestricted with no owner, by config", e.Promoter, e.PromotedByAgentID)
		}
		return
	}
	t.Fatalf("no active entry for review in %+v", entries)
}

// RFC GS P3: while the yaml holds a team, the shared tier's copy is edited
// there, not over the API — every edit is refused and names the yaml key.
// Reads and runs are not edits.
func TestConfigTeamGuard_EveryEditOfAConfiguredSharedTeamIsRefused(t *testing.T) {
	tool, shared, done := teamDefFixture(t)
	defer done()
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })
	reconcile(t, tool, configTeams("review", validTeamGraph))
	defID := sharedActive(t, tool, "review").DefID

	for _, call := range []string{
		`{"op":"create","name":"review","overlay":` + cappedTeamGraph + `}`,
		`{"op":"fork","name":"review","overlay":{"max_iterations":4}}`,
		`{"op":"promote","def_id":"` + defID + `"}`,
		`{"op":"retire","def_id":"` + defID + `","retired":true}`,
		`{"op":"delete","name":"review"}`,
	} {
		res, _ := tool.Execute(shared, json.RawMessage(call))
		if !res.IsError || !strings.Contains(res.Text, "teams.review") || !strings.Contains(res.Text, "change it there") {
			t.Errorf("%s = %s, want refused pointing at teams.review", call, res.Text)
		}
	}
	if row := sharedActive(t, tool, "review"); row.DefID != defID || row.Retired {
		t.Errorf("after the refused edits active = %+v, want the configured version, live", row)
	}

	for _, call := range []string{
		`{"op":"get","def_id":"` + defID + `"}`,
		`{"op":"list","name":"review"}`,
		`{"op":"verify","name":"review"}`,
		`{"op":"render_diagram","name":"review"}`,
		`{"op":"run","name":"review","input":"x"}`,
	} {
		if res, _ := tool.Execute(shared, json.RawMessage(call)); res.IsError {
			t.Errorf("%s refused: %s", call, res.Text)
		}
	}
}

// A tenant forks a configured team into its own tenant as it would any shared
// team; the fork is the tenant's to edit.
func TestConfigTeamGuard_ATenantForksAConfiguredTeamAndEditsItsFork(t *testing.T) {
	tool, _, done := teamDefFixture(t)
	defer done()
	reconcile(t, tool, configTeams("review", validTeamGraph))
	acme := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_acme", TenantID: "acme"})

	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"fork","name":"review","overlay":{"max_iterations":4}}`))
	if res.IsError {
		t.Fatalf("acme's fork of a configured team: %s", res.Text)
	}
	forkID, _ := decodeResult(t, res.Text)["def_id"].(string)
	if _, ok := decodeResult(t, res.Text)["source"]; ok {
		t.Errorf("acme's fork carries source: %s", res.Text)
	}
	if res, _ := tool.Execute(acme, json.RawMessage(`{"op":"retire","def_id":"`+forkID+`","retired":true}`)); res.IsError {
		t.Errorf("acme retiring its own fork: %s", res.Text)
	}
	if res, _ := tool.Execute(acme, json.RawMessage(`{"op":"delete","name":"review"}`)); res.IsError {
		t.Errorf("acme deleting its own fork: %s", res.Text)
	}
}

// A configured team that left the yaml (its version retired) is the
// operator's again: the name may be edited over the API.
func TestConfigTeamGuard_ATeamThatLeftTheYAMLIsEditableAgain(t *testing.T) {
	tool, shared, done := teamDefFixture(t)
	defer done()
	reconcile(t, tool, configTeams("review", validTeamGraph))
	reconcile(t, tool, nil)
	if res, _ := tool.Execute(shared, json.RawMessage(`{"op":"create","name":"review","overlay":`+cappedTeamGraph+`}`)); res.IsError {
		t.Errorf("create over a name the yaml dropped: %s", res.Text)
	}
}
