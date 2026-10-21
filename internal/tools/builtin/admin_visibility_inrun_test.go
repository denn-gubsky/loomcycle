package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A substrate:admin crosses tenant boundaries in the def tools — it reads,
// retires, promotes and forks any tenant's definitions — but only from outside
// a run. A run started with an admin token carries the principal into its
// model's tool calls, and AgentDef, SkillDef, TeamDef, Evaluation and Context
// are agent tools, so without this a prompt-injected run could retire another
// tenant's agents. Inside a run an admin principal sees and acts on its own
// tenant only, like any caller.

func TestDefCallerIsAdmin_HeldOnlyOffRun(t *testing.T) {
	admin := adminOf(asTenant(context.Background(), "acme"), "acme")
	if !defCallerIsAdmin(admin) {
		t.Error("an off-run admin principal is not the admin")
	}
	if defCallerIsAdmin(inAdminRun(admin)) {
		t.Error("an admin principal inside a run crosses tenants")
	}
}

// AgentDef, SkillDef and TeamDef: another tenant's definition, addressed by
// def_id from inside an admin's run, reads as missing and cannot be retired or
// promoted. The same calls off-run still reach it.
func TestDefTools_InRunAdminCannotReadRetireOrPromoteAnotherTenantsDef(t *testing.T) {
	for _, k := range forkKinds()[:3] { // AgentDef, SkillDef, TeamDef
		t.Run(k.kind, func(t *testing.T) {
			tool, base, done := k.fixture(t)
			defer done()
			globexID := createDefFor(t, tool, asTenant(base, "globex"), "globex-secret", k.create)
			admin := adminOf(asTenant(base, "acme"), "acme")
			inRun := inAdminRun(admin)

			for _, op := range []string{
				`{"op":"get","def_id":"` + globexID + `"}`,
				`{"op":"retire","def_id":"` + globexID + `","retired":true}`,
				`{"op":"promote","def_id":"` + globexID + `"}`,
			} {
				if res := execDef(t, tool, inRun, op); !res.IsError || !strings.Contains(res.Text, "not found") {
					t.Errorf("in-run admin %s = %s, want not found", op, res.Text)
				}
			}
			if retired := defRetired(t, tool, globexID); retired {
				t.Fatal("an in-run admin retired another tenant's def")
			}
			if res := execDef(t, tool, inRun, `{"op":"list","name":"globex-secret"}`); res.IsError || strings.Contains(res.Text, globexID) {
				t.Errorf("in-run admin list shows another tenant's def: %s", res.Text)
			}

			if res := execDef(t, tool, admin, `{"op":"get","def_id":"`+globexID+`"}`); res.IsError {
				t.Errorf("off-run admin get: %s", res.Text)
			}
			if res := execDef(t, tool, admin, `{"op":"retire","def_id":"`+globexID+`","retired":true}`); res.IsError {
				t.Errorf("off-run admin retire: %s", res.Text)
			}
		})
	}
}

// defRetired reads the stored retired bit of an AgentDef, SkillDef or TeamDef.
func defRetired(t *testing.T, tool tools.Tool, defID string) bool {
	t.Helper()
	ctx := context.Background()
	var (
		retired bool
		err     error
	)
	switch tl := tool.(type) {
	case *AgentDef:
		var r store.AgentDefRow
		r, err = tl.Store.AgentDefGet(ctx, defID)
		retired = r.Retired
	case *SkillDef:
		var r store.SkillDefRow
		r, err = tl.Store.SkillDefGet(ctx, defID)
		retired = r.Retired
	case *TeamDef:
		var r store.TeamDefRow
		r, err = tl.Store.TeamDefGet(ctx, defID)
		retired = r.Retired
	default:
		t.Fatalf("no retired reader for %T", tool)
	}
	if err != nil {
		t.Fatalf("read %s: %v", defID, err)
	}
	return retired
}

// Every def tool's fork takes a parent_def_id; inside an admin's run another
// tenant's private parent is refused as it is for any caller.
func TestDefFork_InRunAdminCannotForkAnotherTenantsParent(t *testing.T) {
	for _, k := range forkKinds() {
		t.Run(k.kind, func(t *testing.T) {
			tool, base, done := k.fixture(t)
			defer done()
			globexID := createDefFor(t, tool, asTenant(base, "globex"), "globex-own", k.create)
			admin := adminOf(asTenant(base, "acme"), "acme")
			if res := execDef(t, tool, inAdminRun(admin), forkInput("globex-own", globexID, k.fork)); !res.IsError || !strings.Contains(res.Text, "not found") {
				t.Errorf("in-run admin forked another tenant's def: %s", res.Text)
			}
			if res := execDef(t, tool, admin, forkInput("globex-own", globexID, k.fork)); res.IsError {
				t.Errorf("off-run admin fork: %s", res.Text)
			}
		})
	}
}

// Evaluation: an admin's run reads another tenant's evaluations like missing
// ones; the admin off-run still reads them.
func TestEvaluationTool_InRunAdminReadsOnlyItsOwnTenant(t *testing.T) {
	f := newEvalTenantFixture(t)
	p := auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}
	admin := callerCtx("acme", "a_op", &p, "read_any")
	for _, input := range []string{
		`{"op":"get","eval_id":"` + f.evals["globex"] + `"}`,
		`{"op":"list_for_run","run_id":"` + f.runs["globex"] + `"}`,
		`{"op":"list_for_def","def_id":"def_globex"}`,
	} {
		if res := f.exec(t, inAdminRun(admin), input); strings.Contains(res.Text, "globex secret") {
			t.Errorf("in-run admin %s read another tenant's evaluation: %s", input, res.Text)
		}
		if res := f.exec(t, admin, input); res.IsError || !strings.Contains(res.Text, "globex secret") {
			t.Errorf("off-run admin %s = %s, want globex's evaluation", input, res.Text)
		}
	}
}

// Context op=lineage: another tenant's def reads as missing inside an admin's
// run.
func TestContextTool_InRunAdminLineageOfAnotherTenantsDefIsNotFound(t *testing.T) {
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "lineage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.AgentDefCreate(context.Background(), store.AgentDefRow{
		DefID: "def_globex", Name: "worker", TenantID: "globex", Definition: json.RawMessage(`{"system_prompt":"p"}`),
	}); err != nil {
		t.Fatal(err)
	}
	tool := &Context{Store: s}
	admin := adminOf(asTenant(context.Background(), "acme"), "acme")
	in := json.RawMessage(`{"op":"lineage","def_id":"def_globex"}`)
	if res, _ := tool.Execute(inAdminRun(admin), in); !res.IsError || !strings.Contains(res.Text, "not found") {
		t.Errorf("in-run admin lineage of another tenant's def = %s, want not found", res.Text)
	}
	if res, _ := tool.Execute(admin, in); res.IsError {
		t.Errorf("off-run admin lineage: %s", res.Text)
	}
}
