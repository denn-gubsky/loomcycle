package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An evaluation belongs to the tenant of the run it scores. read_any and
// submit_any are capability grants, not tenant grants: a caller holding them
// in acme must find globex's evaluations, runs and defs exactly as it finds
// ids that do not exist.

// evalTenantFixture is a store with a shared def, an acme def and a globex def
// forked from the shared one, and evaluations of acme and globex runs.
type evalTenantFixture struct {
	tool  *Evaluation
	st    store.Store
	runs  map[string]string // label → run id
	evals map[string]string // label → eval id
}

func newEvalTenantFixture(t *testing.T) *evalTenantFixture {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for _, d := range []store.AgentDefRow{
		{DefID: "def_shared", Name: "worker"},
		{DefID: "def_acme", Name: "worker", TenantID: "acme"},
		{DefID: "def_globex", Name: "worker", TenantID: "globex", ParentDefID: "def_shared"},
	} {
		d.Definition = json.RawMessage(`{"system_prompt":"p"}`)
		if _, err := s.AgentDefCreate(ctx, d); err != nil {
			t.Fatalf("AgentDefCreate %s: %v", d.DefID, err)
		}
	}
	f := &evalTenantFixture{
		tool:  &Evaluation{Store: s},
		st:    s,
		runs:  map[string]string{},
		evals: map[string]string{},
	}
	for _, r := range []struct{ label, tenant, agentID, defID string }{
		{"acme", "acme", "a_acme", "def_shared"},
		{"acme_own_def", "acme", "a_acme2", "def_acme"},
		{"globex", "globex", "a_globex", "def_globex"},
		{"globex_shared_def", "globex", "a_globex2", "def_shared"},
	} {
		sess, err := s.CreateSession(ctx, r.tenant, "worker", "u_"+r.tenant)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: r.agentID, TenantID: r.tenant, AgentDefID: r.defID})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		f.runs[r.label] = run.ID
	}
	for _, e := range []struct {
		label, run, def, rationale string
		score                      float64
	}{
		{"acme", "acme", "def_shared", "acme notes", 0.2},
		{"acme_own_def", "acme_own_def", "def_acme", "acme own notes", 0.4},
		{"globex", "globex", "def_globex", "globex secret", 0.9},
		{"globex_shared_def", "globex_shared_def", "def_shared", "globex shared secret", 0.7},
	} {
		id := "eval_" + e.label
		if _, err := s.EvaluationSubmit(ctx, store.EvaluationRow{
			EvalID: id, RunID: f.runs[e.run], DefID: e.def, Score: e.score,
			Rationale: e.rationale, EmitterRole: "self",
		}); err != nil {
			t.Fatalf("EvaluationSubmit %s: %v", id, err)
		}
		f.evals[e.label] = id
		time.Sleep(time.Millisecond) // distinct created_at
	}
	return f
}

// callerCtx is an in-loop caller in tenant, holding the given evaluation
// scopes, with an optional principal on ctx.
func callerCtx(tenant, agentID string, p *auth.Principal, scopes ...string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: agentID, UserID: "u", TenantID: tenant})
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{Scopes: scopes})
	if p != nil {
		ctx = auth.WithPrincipal(ctx, *p)
	}
	return ctx
}

func acmeReader() context.Context {
	return callerCtx("acme", "a_reader", &auth.Principal{TenantID: "acme", Subject: "svc", Scopes: []string{auth.ScopeTenant}}, "read_any", "submit_any")
}

func (f *evalTenantFixture) exec(t *testing.T, ctx context.Context, input string) tools.Result {
	t.Helper()
	res, err := f.tool.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute(%s): %v", input, err)
	}
	return res
}

func (f *evalTenantFixture) submitsFor(t *testing.T, runLabel string) int {
	t.Helper()
	rows, err := f.st.EvaluationListForRun(context.Background(), f.runs[runLabel], 0)
	if err != nil {
		t.Fatalf("EvaluationListForRun: %v", err)
	}
	return len(rows)
}

func TestEvaluationTool_AnotherTenantsReadsLikeAMissingOne(t *testing.T) {
	f := newEvalTenantFixture(t)
	ctx := acmeReader()
	for _, tc := range []struct {
		name, theirs, missing string
		input                 func(id string) string
	}{
		{"get", f.evals["globex"], "eval_nope", func(id string) string { return `{"op":"get","eval_id":"` + id + `"}` }},
		{"list_for_run", f.runs["globex"], "r_nope", func(id string) string { return `{"op":"list_for_run","run_id":"` + id + `"}` }},
		{"list_for_def", "def_globex", "def_nope", func(id string) string { return `{"op":"list_for_def","def_id":"` + id + `"}` }},
		{"aggregate", "def_globex", "def_nope", func(id string) string { return `{"op":"aggregate","def_id":"` + id + `"}` }},
		// globex's def forks the shared one, which acme's run is scored under:
		// a lineage walk from it must not count acme's score there either.
		{"aggregate with lineage", "def_globex", "def_nope", func(id string) string {
			return `{"op":"aggregate","def_id":"` + id + `","include_lineage":true}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			theirs := f.exec(t, ctx, tc.input(tc.theirs))
			none := f.exec(t, ctx, tc.input(tc.missing))
			got := strings.ReplaceAll(theirs.Text, tc.theirs, "<id>")
			want := strings.ReplaceAll(none.Text, tc.missing, "<id>")
			if got != want || theirs.IsError != none.IsError {
				t.Errorf("globex's reads differently from a missing id:\n theirs: %v %s\n none:   %v %s", theirs.IsError, got, none.IsError, want)
			}
			if strings.Contains(got, "globex") || strings.Contains(got, "acme") {
				t.Errorf("response carries evaluation data it must not: %s", got)
			}
		})
	}
}

func TestEvaluationTool_SharedDefReadsOnlyTheCallersTenantsEvaluations(t *testing.T) {
	f := newEvalTenantFixture(t)
	ctx := acmeReader()

	list := f.exec(t, ctx, `{"op":"list_for_def","def_id":"def_shared","limit":1}`)
	if list.IsError {
		t.Fatalf("list_for_def: %s", list.Text)
	}
	// globex's evaluation of the shared def is the newest; a one-row page
	// must still hold acme's.
	if !strings.Contains(list.Text, f.evals["acme"]) || strings.Contains(list.Text, "globex") {
		t.Errorf("acme's page of the shared def = %s, want acme's evaluation only", list.Text)
	}

	agg := f.exec(t, ctx, `{"op":"aggregate","def_id":"def_shared"}`)
	if agg.IsError {
		t.Fatalf("aggregate: %s", agg.Text)
	}
	var out store.AggregateResult
	if err := json.Unmarshal([]byte(agg.Text), &out); err != nil {
		t.Fatalf("decode aggregate: %v", err)
	}
	if out.Count != 1 || out.Score.Mean != 0.2 {
		t.Errorf("acme's aggregate of the shared def = count %d mean %v, want acme's one score 0.2", out.Count, out.Score.Mean)
	}
}

func TestEvaluationTool_OwnTenantReadsItsEvaluations(t *testing.T) {
	f := newEvalTenantFixture(t)
	ctx := acmeReader()
	for input, want := range map[string]string{
		`{"op":"get","eval_id":"` + f.evals["acme"] + `"}`:                "acme notes",
		`{"op":"list_for_run","run_id":"` + f.runs["acme_own_def"] + `"}`: "acme own notes",
		`{"op":"list_for_def","def_id":"def_acme"}`:                       "acme own notes",
		`{"op":"aggregate","def_id":"def_acme"}`:                          `"count":1`,
	} {
		res := f.exec(t, ctx, input)
		if res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s = %s, want it to contain %q", input, res.Text, want)
		}
	}
}

// A substrate:admin — including the legacy shared-secret principal, which
// carries the admin scope — reads every tenant's evaluations, as it reads
// every tenant's defs.
func TestEvaluationTool_AdminReadsEveryTenantsEvaluations(t *testing.T) {
	f := newEvalTenantFixture(t)
	for name, p := range map[string]auth.Principal{
		"admin":  {TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin}},
		"legacy": {TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := callerCtx(p.TenantID, "a_op", &p, "read_any")
			for input, want := range map[string]string{
				`{"op":"get","eval_id":"` + f.evals["globex"] + `"}`:              "globex secret",
				`{"op":"list_for_run","run_id":"` + f.runs["globex"] + `"}`:       "globex secret",
				`{"op":"list_for_def","def_id":"def_shared"}`:                     "globex shared secret",
				`{"op":"aggregate","def_id":"def_shared"}`:                        `"count":2`,
				`{"op":"aggregate","def_id":"def_globex","include_lineage":true}`: `"count":3`,
			} {
				res := f.exec(t, ctx, input)
				if res.IsError || !strings.Contains(res.Text, want) {
					t.Errorf("%s = %s, want it to contain %q", input, res.Text, want)
				}
			}
		})
	}
}

// A submit against another tenant's run is refused as if the run did not
// exist — before the emitter-role check, whose refusal would differ — and
// writes nothing. The submit_self case is the one that wrote: an acme emitter
// whose agent id matches the globex run's agent derived role "self".
func TestEvaluationTool_SubmitAgainstAnotherTenantsRunReadsLikeAMissingOne(t *testing.T) {
	f := newEvalTenantFixture(t)
	for name, ctx := range map[string]context.Context{
		"submit_any":  callerCtx("acme", "a_reader", nil, "submit_any"),
		"submit_self": callerCtx("acme", "a_globex", nil, "submit_self"),
	} {
		t.Run(name, func(t *testing.T) {
			theirs := f.exec(t, ctx, `{"op":"submit","run_id":"`+f.runs["globex"]+`","score":0.1}`)
			none := f.exec(t, ctx, `{"op":"submit","run_id":"r_nope","score":0.1}`)
			got := strings.ReplaceAll(theirs.Text, f.runs["globex"], "<id>")
			want := strings.ReplaceAll(none.Text, "r_nope", "<id>")
			if !theirs.IsError || got != want {
				t.Errorf("submit against globex's run = %v %s, want the missing-run refusal %s", theirs.IsError, got, want)
			}
			if n := f.submitsFor(t, "globex"); n != 1 {
				t.Errorf("globex's run holds %d evaluations, want its own 1", n)
			}
		})
	}
}

func TestEvaluationTool_SubmitAgainstOwnTenantsRunIsRecorded(t *testing.T) {
	f := newEvalTenantFixture(t)
	res := f.exec(t, callerCtx("acme", "a_reader", nil, "submit_any"), `{"op":"submit","run_id":"`+f.runs["acme"]+`","score":0.5}`)
	if res.IsError {
		t.Fatalf("submit: %s", res.Text)
	}
	if n := f.submitsFor(t, "acme"); n != 2 {
		t.Errorf("acme's run holds %d evaluations, want 2", n)
	}
}

// An isolated member's run is confined to its own user inside its tenant:
// read_any and submit_any reach its own runs' evaluations only. Another acme
// member's run reads exactly like a run that does not exist, and a read by def
// — cross-user by nature — answers as an unknown def does.

// addUserRun adds an acme run for user, under def_acme, scored once with
// rationale, and returns the run id and eval id.
func (f *evalTenantFixture) addUserRun(t *testing.T, user, agentID, rationale string) (string, string) {
	t.Helper()
	ctx := context.Background()
	sess, err := f.st.CreateSession(ctx, "acme", "worker", user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := f.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: "acme", AgentDefID: "def_acme"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	evalID := "eval_" + user
	if _, err := f.st.EvaluationSubmit(ctx, store.EvaluationRow{
		EvalID: evalID, RunID: run.ID, DefID: "def_acme", Score: 0.3, Rationale: rationale, EmitterRole: "self",
	}); err != nil {
		t.Fatalf("EvaluationSubmit: %v", err)
	}
	return run.ID, evalID
}

// isolatedCtx is an isolated member's run (bob in acme) holding scopes; p is
// its principal.
func isolatedCtx(agentID string, p auth.Principal, scopes ...string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: agentID, UserID: "bob", TenantID: "acme", Isolated: true})
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{Scopes: scopes})
	return auth.WithPrincipal(ctx, p)
}

func isolatedBob(agentID string, scopes ...string) context.Context {
	return isolatedCtx(agentID, auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser}}, scopes...)
}

func TestEvaluationTool_IsolatedRunReadsAnotherUsersRunLikeAMissingOne(t *testing.T) {
	f := newEvalTenantFixture(t)
	aliceRun, aliceEval := f.addUserRun(t, "alice", "a_alice", "alice private notes")
	for name, ctx := range map[string]context.Context{
		"isolated member": isolatedBob("a_bob", "read_any", "submit_any"),
		// The isolated bit fails closed even under a principal that would
		// otherwise see every tenant.
		"isolated under admin": isolatedCtx("a_bob", auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeAdmin}}, "read_any", "submit_any"),
	} {
		for _, tc := range []struct {
			op, theirs, missing string
			input               func(id string) string
		}{
			{"get", aliceEval, "eval_nope", func(id string) string { return `{"op":"get","eval_id":"` + id + `"}` }},
			{"list_for_run", aliceRun, "r_nope", func(id string) string { return `{"op":"list_for_run","run_id":"` + id + `"}` }},
		} {
			t.Run(name+"/"+tc.op, func(t *testing.T) {
				theirs := f.exec(t, ctx, tc.input(tc.theirs))
				none := f.exec(t, ctx, tc.input(tc.missing))
				got := strings.ReplaceAll(theirs.Text, tc.theirs, "<id>")
				want := strings.ReplaceAll(none.Text, tc.missing, "<id>")
				if got != want || theirs.IsError != none.IsError {
					t.Errorf("alice's reads differently from a missing id:\n theirs: %v %s\n none:   %v %s", theirs.IsError, got, none.IsError, want)
				}
				if strings.Contains(got, "alice") {
					t.Errorf("response carries alice's evaluation data: %s", got)
				}
			})
		}
	}
}

func TestEvaluationTool_IsolatedRunReadsByDefLikeAnUnknownDef(t *testing.T) {
	f := newEvalTenantFixture(t)
	f.addUserRun(t, "alice", "a_alice", "alice private notes")
	f.addUserRun(t, "bob", "a_bob", "bob own notes")
	ctx := isolatedBob("a_bob", "read_any")
	for _, tc := range []struct {
		name  string
		input func(id string) string
	}{
		{"list_for_def", func(id string) string { return `{"op":"list_for_def","def_id":"` + id + `"}` }},
		{"aggregate", func(id string) string { return `{"op":"aggregate","def_id":"` + id + `"}` }},
		{"aggregate with lineage", func(id string) string { return `{"op":"aggregate","def_id":"` + id + `","include_lineage":true}` }},
	} {
		for _, def := range []string{"def_acme", "def_shared"} {
			t.Run(tc.name+"/"+def, func(t *testing.T) {
				theirs := f.exec(t, ctx, tc.input(def))
				none := f.exec(t, ctx, tc.input("def_nope"))
				got := strings.ReplaceAll(theirs.Text, def, "<id>")
				want := strings.ReplaceAll(none.Text, "def_nope", "<id>")
				if got != want || theirs.IsError != none.IsError {
					t.Errorf("%s reads differently from an unknown def:\n got:  %v %s\n none: %v %s", def, theirs.IsError, got, none.IsError, want)
				}
				if strings.Contains(got, "notes") {
					t.Errorf("response carries evaluation data: %s", got)
				}
			})
		}
	}
}

// A submit against another member's run is refused as a missing run and
// writes nothing — with submit_self too, where the emitter's agent id matches
// the target run's agent and the role would derive "self".
func TestEvaluationTool_IsolatedSubmitAgainstAnotherUsersRunReadsLikeAMissingOne(t *testing.T) {
	f := newEvalTenantFixture(t)
	aliceRun, _ := f.addUserRun(t, "alice", "a_alice", "alice private notes")
	for name, ctx := range map[string]context.Context{
		"submit_any":  isolatedBob("a_bob", "submit_any"),
		"submit_self": isolatedBob("a_alice", "submit_self"),
	} {
		t.Run(name, func(t *testing.T) {
			theirs := f.exec(t, ctx, `{"op":"submit","run_id":"`+aliceRun+`","score":0.1}`)
			none := f.exec(t, ctx, `{"op":"submit","run_id":"r_nope","score":0.1}`)
			got := strings.ReplaceAll(theirs.Text, aliceRun, "<id>")
			want := strings.ReplaceAll(none.Text, "r_nope", "<id>")
			if !theirs.IsError || got != want {
				t.Errorf("submit against alice's run = %v %s, want the missing-run refusal %s", theirs.IsError, got, want)
			}
			rows, err := f.st.EvaluationListForRun(context.Background(), aliceRun, 0)
			if err != nil {
				t.Fatalf("EvaluationListForRun: %v", err)
			}
			if len(rows) != 1 {
				t.Errorf("alice's run holds %d evaluations, want her own 1", len(rows))
			}
		})
	}
}

func TestEvaluationTool_IsolatedRunReadsAndScoresItsOwnRuns(t *testing.T) {
	f := newEvalTenantFixture(t)
	bobRun, bobEval := f.addUserRun(t, "bob", "a_bob", "bob own notes")
	ctx := isolatedBob("a_bob", "read_any", "submit_any")
	for input, want := range map[string]string{
		`{"op":"get","eval_id":"` + bobEval + `"}`:        "bob own notes",
		`{"op":"list_for_run","run_id":"` + bobRun + `"}`: "bob own notes",
	} {
		res := f.exec(t, ctx, input)
		if res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s = %s, want it to contain %q", input, res.Text, want)
		}
	}
	if res := f.exec(t, ctx, `{"op":"submit","run_id":"`+bobRun+`","score":0.5}`); res.IsError {
		t.Fatalf("submit on own run: %s", res.Text)
	}
	rows, err := f.st.EvaluationListForRun(context.Background(), bobRun, 0)
	if err != nil {
		t.Fatalf("EvaluationListForRun: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("bob's run holds %d evaluations, want 2", len(rows))
	}
}

// A non-isolated member of the same tenant keeps its tenant-wide reach.
func TestEvaluationTool_NonIsolatedTenantCallerReadsEveryUsersEvaluations(t *testing.T) {
	f := newEvalTenantFixture(t)
	aliceRun, aliceEval := f.addUserRun(t, "alice", "a_alice", "alice private notes")
	ctx := callerCtx("acme", "a_bob", &auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}}, "read_any", "submit_any")
	for input, want := range map[string]string{
		`{"op":"get","eval_id":"` + aliceEval + `"}`:        "alice private notes",
		`{"op":"list_for_run","run_id":"` + aliceRun + `"}`: "alice private notes",
		`{"op":"list_for_def","def_id":"def_acme"}`:         "alice private notes",
		`{"op":"aggregate","def_id":"def_acme"}`:            `"count":2`,
	} {
		res := f.exec(t, ctx, input)
		if res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s = %s, want it to contain %q", input, res.Text, want)
		}
	}
	if res := f.exec(t, ctx, `{"op":"submit","run_id":"`+aliceRun+`","score":0.5}`); res.IsError {
		t.Errorf("submit on alice's run: %s", res.Text)
	}
}
