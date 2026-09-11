package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// fullACL is the graph plus the ACL a runnable Starter needs.
func fullACL() string {
	return strings.TrimSuffix(starterGraph, "}") + `,"channels":{"publish":["verdicts"],"subscribe":["pr-events"]}}`
}

func declared(names ...string) func(context.Context) map[string]tools.ChannelDef {
	m := map[string]tools.ChannelDef{}
	for _, n := range names {
		m[n] = tools.ChannelDef{Name: n, Scope: "user"}
	}
	return func(context.Context) map[string]tools.ChannelDef { return m }
}

// TestTeamDefPreflight_RefusesAStarterWithNoACL is the shape that actually
// stranded a workflow: the def parses, validates, saves, promotes — and then
// the first run says it cannot reach its own source. Now it never gets stored.
func TestTeamDefPreflight_RefusesAStarterWithNoACL(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"create","name":"triage","overlay":`+starterGraph+`}`))
	if !res.IsError {
		t.Fatal("a Starter with no channels block was accepted — it could never have run")
	}
	// The refusal must be ACTIONABLE: which state, which field, and the exact
	// block to paste. A preflight that only moves the discovery earlier without
	// making the fix cheaper has done half a job.
	for _, want := range []string{`state "wave"`, "source", `"channels"`, `"publish": ["verdicts"]`, `"subscribe": ["pr-events"]`} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("refusal is missing %q:\n%s", want, res.Text)
		}
	}
	// And nothing was stored — a refusal that persisted a broken def would be
	// worse than no refusal, because the row would then be forkable.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"triage"}`))
	versions, _ := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 0 {
		t.Errorf("a refused create left %d version(s) behind: %s", len(versions), res.Text)
	}
}

// TestTeamDefPreflight_RefusesAHalfACL: the sink side is checked as
// independently as the source side.
func TestTeamDefPreflight_RefusesAHalfACL(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"create","name":"triage","overlay":`+
			strings.TrimSuffix(starterGraph, "}")+`,"channels":{"subscribe":["pr-events"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, "does not grant publish") {
		t.Fatalf("a sink with no publish grant was accepted: IsError=%v %s", res.IsError, res.Text)
	}
}

// TestTeamDefPreflight_RefusesAnUndeclaredChannel — with a catalog wired, a
// channel that does not exist is refused with the yaml to declare it.
func TestTeamDefPreflight_RefusesAnUndeclaredChannel(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events") // "verdicts" is missing
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"triage","overlay":`+fullACL()+`}`))
	if !res.IsError {
		t.Fatal("a def naming an undeclared channel was accepted")
	}
	for _, want := range []string{`channel "verdicts"`, "not declared", "channels:", "scope: user", "ChannelDef op=create"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("refusal is missing %q:\n%s", want, res.Text)
		}
	}
}

// TestTeamDefPreflight_SkipsTheDeclaredCheckWithNoCatalog: a tool that cannot
// see the declarations must not refuse — it cannot tell "undeclared" from "I
// have no list", and refusing on the second breaks every create on a plane
// that wired no catalog.
func TestTeamDefPreflight_SkipsTheDeclaredCheckWithNoCatalog(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = nil
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"triage","overlay":`+fullACL()+`}`))
	if res.IsError {
		t.Fatalf("no catalog wired must SKIP the declared-set check, not fail it: %s", res.Text)
	}
	// The ACL half still runs — it needs no catalog.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"other","overlay":`+starterGraph+`}`))
	if !res.IsError || !strings.Contains(res.Text, "does not grant") {
		t.Errorf("the ACL check must run with no catalog: IsError=%v %s", res.IsError, res.Text)
	}
}

// TestTeamDefPreflight_AcceptsAWellFormedDef — the preflight must not become a
// reason a correct workflow cannot be written.
func TestTeamDefPreflight_AcceptsAWellFormedDef(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events", "verdicts")
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"triage","overlay":`+fullACL()+`}`))
	if res.IsError {
		t.Fatalf("a correct def was refused: %s", res.Text)
	}
	// And a graph with no channels at all is untouched by any of this.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"plain","overlay":`+linearBoardTeam+`}`))
	if res.IsError {
		t.Fatalf("a channel-free team was refused: %s", res.Text)
	}
}

// TestTeamDefPreflight_RunsOnFork: a fork is an authoring act too, and an
// overlay replaces `channels` wholesale — so a fork is exactly how a good def
// becomes a broken one.
func TestTeamDefPreflight_RunsOnFork(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events", "verdicts")
	ctx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})
	createTeam(t, tool, ctx, "triage", fullACL())

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"fork","name":"triage","overlay":{"channels":{"subscribe":["pr-events"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, "does not grant publish") {
		t.Fatalf("a fork that dropped the publish grant was accepted: IsError=%v %s", res.IsError, res.Text)
	}
}

// TestTeamDefPreflight_AuthorityRefusalWinsOverPreflight pins the ordering:
// "you may not grant this" is a harder refusal than "this will not work", and
// reporting the softer one first would send the author to fix a def they are
// not allowed to write at all.
func TestTeamDefPreflight_AuthorityRefusalWinsOverPreflight(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	// The def must fail BOTH checks, or the test cannot tell which ran first:
	// its ACL claims a subscribe grant the author does not hold (authority), AND
	// leaves the sink with no publish grant (preflight).
	narrow := authoringCtx(nil, nil) // holds nothing

	res, _ := tool.Execute(narrow, json.RawMessage(
		`{"op":"create","name":"triage","overlay":`+
			strings.TrimSuffix(starterGraph, "}")+`,"channels":{"subscribe":["pr-events"]}}}`))
	if !res.IsError {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(res.Text, "never widen it") {
		t.Errorf("the AUTHORITY refusal must come first, got:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "does not grant publish") {
		t.Errorf("the preflight refusal preempted the authority one:\n%s", res.Text)
	}
}

// ---- the verify sweep ----

// storeBrokenTeam writes a definition straight to the store, bypassing create.
// That is how a def written BEFORE the preflight existed reaches the sweep —
// and it is the only population the sweep is really for.
func storeBrokenTeam(t *testing.T, tool *TeamDef, ctx context.Context, name, defJSON string) {
	t.Helper()
	def, err := teamgraph.Parse([]byte(defJSON))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	row, err := tool.Store.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: mintTeamDefID(), Name: name, Definition: json.RawMessage(defJSON),
		ContentSHA256: teamgraph.Sign(name, def),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tool.Store.TeamDefSetActive(ctx, "", name, row.DefID, "a_test"); err != nil {
		t.Fatalf("promote: %v", err)
	}
}

// TestTeamDefVerify_ReportsAChannelDeletedAfterTheDefWasWritten: the hash still
// matches and the team is broken. That gap is the whole reason verify sweeps.
func TestTeamDefVerify_ReportsAChannelDeletedAfterTheDefWasWritten(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events", "verdicts")
	actx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})
	created := createTeam(t, tool, actx, "triage", fullACL())
	sha, _ := created["content_sha256"].(string)

	// The operator deletes the channel.
	tool.ChannelCatalog = declared("pr-events")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"triage","content_sha256":"`+sha+`"}`))
	if res.IsError {
		t.Fatalf("the sweep must never turn verify into an error: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["matches"] != true {
		t.Errorf("matches = %v — the def is unchanged, so the hash must still match", out["matches"])
	}
	if out["runnable"] != false {
		t.Errorf("runnable = %v, want false", out["runnable"])
	}
	issues, _ := out["issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want one", out["issues"])
	}
	got, _ := issues[0].(map[string]any)
	if got["kind"] != "channel_undeclared" || got["channel"] != "verdicts" || got["state"] != "wave" {
		t.Errorf("issue = %v, want channel_undeclared on state wave / channel verdicts", got)
	}
}

// TestTeamDefVerify_ReportsAnACLGap covers the def written before the preflight
// existed — the population that is already stored and cannot be fixed by
// refusing new writes.
func TestTeamDefVerify_ReportsAnACLGap(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events", "verdicts")
	storeBrokenTeam(t, tool, ctx, "legacy", starterGraph) // no channels block at all

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"legacy"}`))
	if res.IsError {
		t.Fatalf("verify: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["runnable"] != false {
		t.Fatalf("runnable = %v, want false", out["runnable"])
	}
	kinds := map[string]bool{}
	for _, raw := range out["issues"].([]any) {
		kinds[raw.(map[string]any)["kind"].(string)] = true
	}
	if !kinds["acl_missing"] {
		t.Errorf("issues = %v, want an acl_missing", out["issues"])
	}
}

// TestTeamDefVerify_ReportsARetiredMember: a def can also rot because a member
// stopped resolving.
func TestTeamDefVerify_ReportsARetiredMember(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events", "verdicts")
	tool.AgentExists = func(_ context.Context, name string) bool { return name != "reviewer" }
	actx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})
	createTeam(t, tool, actx, "triage", fullACL())

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"triage"}`))
	out := decodeResult(t, res.Text)
	if out["runnable"] != false {
		t.Fatalf("runnable = %v, want false; issues=%v", out["runnable"], out["issues"])
	}
	got := out["issues"].([]any)[0].(map[string]any)
	if got["kind"] != "agent_missing" || got["agent"] != "reviewer" {
		t.Errorf("issue = %v, want agent_missing for reviewer", got)
	}
}

// TestTeamDefVerify_HealthyTeamKeepsTheOldShape: `issues` appears only when
// there are some, so an existing caller parsing the response sees no change;
// `runnable` is reported either way, because an absent field could not be told
// apart from a sweep that did not run.
func TestTeamDefVerify_HealthyTeamKeepsTheOldShape(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events", "verdicts")
	tool.AgentExists = func(context.Context, string) bool { return true }
	actx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})
	created := createTeam(t, tool, actx, "triage", fullACL())
	sha, _ := created["content_sha256"].(string)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"triage","content_sha256":"`+sha+`"}`))
	out := decodeResult(t, res.Text)
	if _, present := out["issues"]; present {
		t.Errorf("a healthy team reported issues: %v", out["issues"])
	}
	if out["runnable"] != true || out["matches"] != true || out["deployed"] != true {
		t.Errorf("healthy verify = %v", out)
	}
}

// TestTeamDefVerify_UndeployedTeamIsUnchanged: the not-found branch returns
// before any sweep, so it keeps its exact shape.
func TestTeamDefVerify_UndeployedTeamIsUnchanged(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pr-events")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"nope"}`))
	out := decodeResult(t, res.Text)
	if out["deployed"] != false {
		t.Fatalf("deployed = %v, want false", out["deployed"])
	}
	if _, present := out["runnable"]; present {
		t.Errorf("an undeployed team must not claim a runnable verdict: %v", out)
	}
}
