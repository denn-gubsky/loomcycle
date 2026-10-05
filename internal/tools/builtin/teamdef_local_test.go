package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// localTeamFixture is a TeamDef tool wired to an AgentDef tool over one store,
// as main.go wires them, plus a caller holding every agent-authoring grant and
// the tools Read, Grep and Agent.
func localTeamFixture(t *testing.T) (*TeamDef, context.Context) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := &config.Config{Agents: map[string]config.AgentDef{
		"static-one":  {Tier: "middle"},
		"sdlc/static": {Tier: "middle"},
	}}
	cfg.Env.CodeAgentsEnabled = true
	agents := &AgentDef{Store: s, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192, MaxCodeBytes: 262144}
	tool := &TeamDef{Store: s, MaxDefinitionBytes: 1 << 20, MaxDescriptionBytes: 8192, Agents: agents}
	return tool, localAuthorCtx("", []string{"any"}, []string{"Read", "Grep", "Agent"})
}

// localAuthorCtx is a caller in tenantID with the given agent_def_scopes and tools.
func localAuthorCtx(tenantID string, scopes, callerTools []string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_author", TenantID: tenantID})
	ctx = tools.WithAgentName(ctx, "author")
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{Scopes: scopes, SelfName: "author"})
	return tools.WithAgentTools(ctx, callerTools)
}

// localTeam is a one-state team running its own agent "reviewer", whose body
// is the given overlay.
func localTeam(body string) string {
	return `{"entry":"review","local":{"agents":{"reviewer":` + body + `}},
	  "states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},
	            {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"review","to":"done","on":"success"}]}`
}

func teamOp(t *testing.T, tool *TeamDef, ctx context.Context, op, name, overlay string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"`+name+`","overlay":`+overlay+`}`))
	if err != nil {
		t.Fatalf("%s %q: %v", op, name, err)
	}
	return res
}

func wantRefused(t *testing.T, res tools.Result, what string, mentions ...string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("%s: accepted, want a refusal; got %s", what, res.Text)
	}
	for _, m := range mentions {
		if !strings.Contains(res.Text, m) {
			t.Errorf("%s: refusal %q should mention %q", what, res.Text, m)
		}
	}
}

func TestTeamDefCreate_StoresLocalAgentOnlyInTheTeam(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	res := teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle","tools":["Read"],"system_prompt":"You review diffs."}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	row, err := tool.Store.TeamDefGetActive(ctx, "", "sdlc")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		t.Fatalf("stored definition: %v", err)
	}
	if _, ok := def.LocalAgent("reviewer"); !ok {
		t.Fatalf("the stored definition lost its local agent: %s", row.Definition)
	}
	// Nowhere else: not an agent version, not a registered agent.
	for _, name := range []string{"reviewer", "sdlc/reviewer"} {
		if _, err := tool.Store.AgentDefGetActive(ctx, "", name); err == nil {
			t.Errorf("a team's own agent was written to agent_defs as %q", name)
		}
		if _, err := tool.Store.DynamicAgentGet(ctx, "", name); err == nil {
			t.Errorf("a team's own agent was written to dynamic_agents as %q", name)
		}
	}
}

// One refused overlay per gate AgentDef create applies. Each must be refused
// for a team's own agent exactly as it is for a global one — the two share
// gateNewDef — so every row is run through both tools.
func TestTeamDefCreate_LocalAgentPassesEveryAgentDefCreateGate(t *testing.T) {
	for _, tc := range []struct {
		gate    string
		ctx     func(context.Context) context.Context
		body    string
		mention string
	}{
		{"tools ceiling", nil, `{"tier":"middle","tools":["Read","Bash"]}`, "Tools cannot widen"},
		{"agent_def_scopes: default-deny with no policy",
			func(context.Context) context.Context { return localAuthorCtx("", nil, []string{"Read"}) },
			`{"tier":"middle"}`, "agent_def_scopes"},
		{"agent_def_scopes: name outside the grant",
			func(context.Context) context.Context {
				return localAuthorCtx("", []string{"named:other/**"}, []string{"Read"})
			},
			`{"tier":"middle"}`, "agent_def_scopes"},
		{"routing mode", nil, `{"tier":"middle","model":"some-model"}`, "tier"},
		{"tool_choice", nil, `{"tier":"middle","tool_choice":{"mode":"sometimes"}}`, "tool_choice"},
		{"output_format", nil, `{"tier":"middle","output_format":{"type":"yaml"}}`, "output_format"},
		{"inline code compiles", nil, `{"provider":"code-js","code_body":"function ("}`, "does not compile"},
		{"named-scope pattern", nil, `{"tier":"middle","agent_def_scopes":["named:**"]}`, "agent_def_scopes"},
		{"definition size cap", nil, `{"tier":"middle","system_prompt":"` + strings.Repeat("x", 140000) + `"}`, "exceeds max"},
		{"hooks unchangeable from inside a run",
			func(c context.Context) context.Context { return tools.WithRunID(c, "run_1") },
			`{"tier":"middle","hooks":{"run_end":[{"name":"h","url":"https://example.com/h"}]}}`, "cannot be changed from inside a run"},
	} {
		t.Run(tc.gate, func(t *testing.T) {
			tool, ctx := localTeamFixture(t)
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}
			// The gate exists for a global agent of that name...
			global, _ := tool.Agents.Execute(ctx, json.RawMessage(`{"op":"create","name":"sdlc/reviewer","overlay":`+tc.body+`}`))
			wantRefused(t, global, "AgentDef create", tc.mention)
			// ...and a team's own agent does not get past it.
			wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(tc.body)), "TeamDef create", tc.mention, `local.agents["reviewer"]`)
			if _, err := tool.Store.TeamDefGetActive(ctx, "", "sdlc"); err == nil {
				t.Error("a refused team was stored")
			}
		})
	}
}

// The grant is judged under the agent's FULL name, so an operator who granted
// a team's subtree has granted that team's own agents and no other team's.
func TestTeamDefCreate_ScopeGrantIsJudgedUnderTheTeamQualifiedName(t *testing.T) {
	tool, _ := localTeamFixture(t)
	ctx := localAuthorCtx("", []string{"named:sdlc/**"}, []string{"Read"})
	if res := teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)); res.IsError {
		t.Fatalf("named:sdlc/** covers sdlc/reviewer, but create was refused: %s", res.Text)
	}
	wantRefused(t, teamOp(t, tool, ctx, "create", "other", localTeam(`{"tier":"middle"}`)), "team outside the grant", "other/reviewer")
}

// A fork is an authoring act by whoever forks. The agents it carries over
// without resending them are judged under the FORKER's authority, or a caller
// could obtain — and re-prompt through the states it rewrites — an agent it
// could never have authored.
func TestTeamDefFork_InheritedLocalAgentsAreGatedAgainstTheForker(t *testing.T) {
	tool, operator := localTeamFixture(t)
	if res := teamOp(t, tool, operator, "create", "sdlc", localTeam(`{"tier":"middle","tools":["Read","Grep"]}`)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	// The overlay does not mention `local`: the parent's agents ride along.
	const noLocal = `{"max_iterations":3}`

	narrow := localAuthorCtx("", []string{"any"}, []string{"Read"})
	wantRefused(t, teamOp(t, tool, narrow, "fork", "sdlc", noLocal), "fork by a caller without Grep", "Tools cannot widen", "Grep")

	unscoped := localAuthorCtx("", nil, []string{"Read", "Grep"})
	wantRefused(t, teamOp(t, tool, unscoped, "fork", "sdlc", noLocal), "fork by a caller with no agent_def_scopes", "agent_def_scopes")

	rows, err := tool.Store.TeamDefListByName(operator, "sdlc")
	if err != nil || len(rows) != 1 {
		t.Fatalf("a refused fork must write nothing; versions = %d (err %v)", len(rows), err)
	}
	// The same fork by a caller who could have authored the agent is fine.
	if res := teamOp(t, tool, operator, "fork", "sdlc", noLocal); res.IsError {
		t.Fatalf("fork by the original author: %s", res.Text)
	}
}

func TestTeamDefFork_KeepsOrReplacesLocalAgents(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	if res := teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	names := func(res tools.Result) []string {
		t.Helper()
		if res.IsError {
			t.Fatalf("fork: %s", res.Text)
		}
		var out struct {
			Definition json.RawMessage `json:"definition"`
		}
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		def, err := teamgraph.Parse(out.Definition)
		if err != nil {
			t.Fatalf("definition: %v", err)
		}
		return def.LocalAgentNames()
	}
	if got := names(teamOp(t, tool, ctx, "fork", "sdlc", `{"max_iterations":3}`)); len(got) != 1 || got[0] != "reviewer" {
		t.Errorf("a fork that sends no local block must keep the parent's agents; got %v", got)
	}
	replaced := `{"local":{"agents":{"judge":{"tier":"low"}}},
	  "states":[{"state":"review","handler":{"kind":"agent","agent":"./judge"}},{"state":"done","handler":{"kind":"terminal"}}]}`
	if got := names(teamOp(t, tool, ctx, "fork", "sdlc", replaced)); len(got) != 1 || got[0] != "judge" {
		t.Errorf("a fork that sends local.agents must replace the whole list; got %v", got)
	}
	// Dropping the agents while a state still names one is refused.
	wantRefused(t, teamOp(t, tool, ctx, "fork", "sdlc", `{"local":{"agents":{}}}`), "fork removing a referenced agent", `"./reviewer"`)
}

func TestTeamDefCreate_RefusesUnknownLocalKind(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	overlay := strings.Replace(validTeamGraph, `"entry"`, `"local":{"skills":{"s":{}}},"entry"`, 1)
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", overlay), "local.skills", "skills")
}

// A team named before the one-segment rule keeps working, but "<team>/<name>"
// must split one way, so it cannot declare agents of its own.
func TestTeamDefFork_LegacyNamedTeamCannotDeclareLocalAgents(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	if _, err := tool.Store.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: "tdf_legacy", Name: "old/team", Definition: json.RawMessage(validTeamGraph), ContentSHA256: "sha256:x",
	}); err != nil {
		t.Fatalf("seed legacy team: %v", err)
	}
	if err := tool.Store.TeamDefSetActive(ctx, "", "old/team", "tdf_legacy", "a_author", store.TeamDefPromoter{}); err != nil {
		t.Fatalf("activate legacy team: %v", err)
	}
	if res := teamOp(t, tool, ctx, "fork", "old/team", `{"max_iterations":2}`); res.IsError {
		t.Fatalf("a legacy-named team must stay forkable without local agents: %s", res.Text)
	}
	wantRefused(t, teamOp(t, tool, ctx, "fork", "old/team", `{"local":{"agents":{"x":{"tier":"low"}}}}`),
		"local agents on a legacy-named team", "one segment")
}

func TestTeamDefCreate_RefusesLocalAgentsWhenNoAgentDefToolIsWired(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	tool.Agents = nil
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)), "no gates wired", "local")
}

func TestTeamDefCreate_LocalCodeJSAgentMustCarryInlineCode(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"provider":"code-js"}`)), "code-js with no code", "code_body")
	ok := localTeam(`{"provider":"code-js","code_body":"function run(input) { return { final: 'ok' }; }"}`)
	if res := teamOp(t, tool, ctx, "create", "sdlc2", ok); res.IsError {
		t.Fatalf("a local code-js agent with inline code: %s", res.Text)
	}
}

// ---- collisions, both directions ----

func TestTeamDefCreate_RefusesLocalAgentWhoseFullNameIsAGlobalAgent(t *testing.T) {
	seed := map[string]func(t *testing.T, tool *TeamDef, ctx context.Context){
		"a static agent": func(t *testing.T, tool *TeamDef, ctx context.Context) {
			// "sdlc/static" is in the fixture's config.
		},
		"an AgentDef version": func(t *testing.T, tool *TeamDef, ctx context.Context) {
			res, _ := tool.Agents.Execute(ctx, json.RawMessage(`{"op":"create","name":"sdlc/static2","overlay":{"tier":"low"}}`))
			if res.IsError {
				t.Fatalf("seed: %s", res.Text)
			}
		},
		"a registered agent": func(t *testing.T, tool *TeamDef, ctx context.Context) {
			if err := tool.Store.DynamicAgentUpsert(ctx, store.DynamicAgent{Name: "sdlc/static3", Definition: json.RawMessage(`{}`)}); err != nil {
				t.Fatalf("seed: %v", err)
			}
		},
	}
	local := map[string]string{"a static agent": "static", "an AgentDef version": "static2", "a registered agent": "static3"}
	for name, do := range seed {
		t.Run(name, func(t *testing.T) {
			tool, ctx := localTeamFixture(t)
			do(t, tool, ctx)
			overlay := strings.ReplaceAll(localTeam(`{"tier":"middle"}`), "reviewer", local[name])
			wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", overlay), "local clashing with "+name, "already exists", "sdlc/"+local[name])
			// A team of another name has no clash.
			if res := teamOp(t, tool, ctx, "create", "other", overlay); res.IsError {
				t.Errorf("no agent is named other/%s, but create was refused: %s", local[name], res.Text)
			}
		})
	}
}

func TestAgentDefCreate_RefusesNameOfAnActiveTeamsLocalAgent(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	created := decodeResult(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)).Text)
	create := func(name string) tools.Result {
		res, _ := tool.Agents.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":{"tier":"low"}}`))
		return res
	}
	wantRefused(t, create("sdlc/reviewer"), "global agent named like a team's own", "sdlc", "reviewer")
	// Only that exact two-segment name is taken.
	for _, free := range []string{"sdlc/other", "sdlc/reviewer/deep", "reviewer", "sdlc"} {
		if res := create(free); res.IsError {
			t.Errorf("%q does not clash, but create was refused: %s", free, res.Text)
		}
	}
	// Another tenant's team does not reserve the name here.
	other := localAuthorCtx("tenant-b", []string{"any"}, []string{"Read"})
	if res, _ := tool.Agents.Execute(other, json.RawMessage(`{"op":"create","name":"sdlc/reviewer","overlay":{"tier":"low"}}`)); res.IsError {
		t.Errorf("tenant-b has no team sdlc, but create was refused: %s", res.Text)
	}
	// A retired team no longer runs its agents, so the name is free again.
	defID, _ := created["def_id"].(string)
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`)); res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	if err := TeamLocalAgentCollision(ctx, tool.Store, "", "sdlc/reviewer"); err != nil {
		t.Errorf("a retired team still reserves its agent's name: %v", err)
	}
}

func TestTeamDefPromote_RefusesVersionWhoseLocalAgentNowClashes(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	v1 := decodeResult(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)).Text)
	// v2 drops the agent and becomes active, which frees the name...
	v2 := `{"local":{"agents":{}},"states":[{"state":"review","handler":{"kind":"agent","agent":"static-one"}},{"state":"done","handler":{"kind":"terminal"}}]}`
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"sdlc","promote":true,"overlay":`+v2+`}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if res, _ := tool.Agents.Execute(ctx, json.RawMessage(`{"op":"create","name":"sdlc/reviewer","overlay":{"tier":"low"}}`)); res.IsError {
		t.Fatalf("the name is free while v2 is active: %s", res.Text)
	}
	// ...so going back to v1 would put two agents under one name.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+v1["def_id"].(string)+`"}`))
	wantRefused(t, res, "promote of a version whose agent now clashes", "sdlc/reviewer")
}

func TestTeamDefVerify_ReportsUnreferencedLocalAgentWithoutMakingTheTeamUnrunnable(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	tool.AgentExists = func(context.Context, string) bool { return true }
	overlay := strings.Replace(localTeam(`{"tier":"middle"}`), `"agents":{`, `"agents":{"spare":{"tier":"low"},`, 1)
	if res := teamOp(t, tool, ctx, "create", "sdlc", overlay); res.IsError {
		t.Fatalf("an unreferenced local agent is reported, not refused: %s", res.Text)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"sdlc"}`))
	out := decodeResult(t, res.Text)
	if out["runnable"] != true {
		t.Errorf("an unreferenced agent must not make the team unrunnable: %s", res.Text)
	}
	issues, _ := out["issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("want exactly the one advisory issue, got %s", res.Text)
	}
	issue := issues[0].(map[string]any)
	if issue["kind"] != "local_agent_unreferenced" || issue["agent"] != "./spare" {
		t.Errorf("issue = %v", issue)
	}
}

// A restore writes a body without an authoring caller, so it re-runs only the
// body's own checks — the same ones a restored agent def gets.
func TestValidateTeamDefBody_ChecksLocalAgentBodies(t *testing.T) {
	if err := ValidateTeamDefBody(json.RawMessage(localTeam(`{"tier":"middle","tools":["Read"]}`))); err != nil {
		t.Fatalf("a well-formed local agent: %v", err)
	}
	for what, body := range map[string]string{
		"an unknown local kind":       strings.Replace(validTeamGraph, `"entry"`, `"local":{"skills":{}},"entry"`, 1),
		"a pin and a tier at once":    localTeam(`{"tier":"middle","model":"m"}`),
		"hooks on a tool it lacks":    localTeam(`{"tier":"middle","tools":["Read"],"tool_hooks":{"Bash":{"pre":[{"name":"h","url":"https://example.com"}]}}}`),
		"a body that is not an agent": localTeam(`{"tools":"Read"}`),
	} {
		if err := ValidateTeamDefBody(json.RawMessage(body)); err == nil {
			t.Errorf("%s must be refused at restore", what)
		}
	}
}

func TestLocalAgentDefinition_BuildsWhatAgentDefCreateStores(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	const body = `{"tier":"middle","tools":["Read",{"name":"Grep","hooks":{}}],"system_prompt":"You review.","sampling":{"temperature":0}}`
	res, _ := tool.Agents.Execute(ctx, json.RawMessage(`{"op":"create","name":"g","overlay":`+body+`}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	stored, err := tool.Store.AgentDefGetActive(ctx, "", "g")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	local, err := LocalAgentDefinition(json.RawMessage(body))
	if err != nil {
		t.Fatalf("LocalAgentDefinition: %v", err)
	}
	if string(local) != string(stored.Definition) {
		t.Errorf("a team's own agent decodes differently from the same overlay through AgentDef create:\n local: %s\nstored: %s", local, stored.Definition)
	}
}

func TestValidateTeamDefBody_NamesTheRefusedLocalAgent(t *testing.T) {
	err := ValidateTeamDefBody(json.RawMessage(localTeam(`{"tier":"middle","tools":["Read"],"tool_hooks":{"Bash":{"pre":[{"name":"h","url":"https://example.com"}]}}}`)))
	if err == nil || !strings.Contains(err.Error(), `local.agents["reviewer"]`) || !strings.Contains(err.Error(), "Bash") {
		t.Fatalf("want a refusal naming the agent and the tool it lacks, got %v", err)
	}
}

// ---- every path by which the two names can come to coexist ----

func agentOp(t *testing.T, tool *TeamDef, ctx context.Context, input string) tools.Result {
	t.Helper()
	res, err := tool.Agents.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("AgentDef %s: %v", input, err)
	}
	return res
}

// A retired agent does not resolve, so a team may take its name for one of its
// own. Every way of bringing the agent back must then be refused: un-retire,
// promote, and a fork that promotes.
func TestAgentDef_BringingBackANameATeamNowDeclaresIsRefused(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	created := decodeResult(t, agentOp(t, tool, ctx, `{"op":"create","name":"sdlc/reviewer","overlay":{"tier":"low"}}`).Text)
	defID, _ := created["def_id"].(string)
	if res := agentOp(t, tool, ctx, `{"op":"retire","def_id":"`+defID+`","retired":true}`); res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	if res := teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)); res.IsError {
		t.Fatalf("the retired agent no longer holds the name, but the team was refused: %s", res.Text)
	}
	for what, input := range map[string]string{
		"un-retire":         `{"op":"retire","def_id":"` + defID + `","retired":false}`,
		"promote":           `{"op":"promote","def_id":"` + defID + `"}`,
		"fork and promote":  `{"op":"fork","name":"sdlc/reviewer","parent_def_id":"` + defID + `","promote":true,"overlay":{"effort":"low"}}`,
		"fork, not promote": `{"op":"fork","name":"sdlc/reviewer","parent_def_id":"` + defID + `","overlay":{"effort":"low"}}`,
	} {
		wantRefused(t, agentOp(t, tool, ctx, input), what, "sdlc", "reviewer", "own agent")
	}
	row, err := tool.Store.AgentDefGet(ctx, defID)
	if err != nil || !row.Retired {
		t.Errorf("the agent must still be retired (retired=%v, err %v)", row.Retired, err)
	}
	if _, err := tool.Store.AgentDefGetActive(ctx, "", "sdlc/reviewer"); err == nil {
		t.Error("the agent's name resolves again beside the team's own agent")
	}
}

// The mirror: a retired team version does not run its agents, so a global
// agent may take one's name; un-retiring the version must then be refused.
func TestTeamDefRetire_UnretireRefusedWhileItsLocalAgentsNameIsTaken(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	created := decodeResult(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)).Text)
	defID, _ := created["def_id"].(string)
	retire := func(retired string) tools.Result {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":`+retired+`}`))
		return res
	}
	if res := retire("true"); res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	if res := agentOp(t, tool, ctx, `{"op":"create","name":"sdlc/reviewer","overlay":{"tier":"low"}}`); res.IsError {
		t.Fatalf("a retired team does not hold the name, but the agent was refused: %s", res.Text)
	}
	wantRefused(t, retire("false"), "un-retire of a team whose agent's name is now taken", "sdlc/reviewer")
	if row, err := tool.Store.TeamDefGet(ctx, defID); err != nil || !row.Retired {
		t.Errorf("the team version must still be retired (retired=%v, err %v)", row.Retired, err)
	}
}

// A version that declares agents is not made active on a server that cannot
// check them — the same answer create gives.
func TestTeamDefPromote_RefusedWhenLocalAgentsCannotBeChecked(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	created := decodeResult(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)).Text)
	plain := decodeResult(t, teamOp(t, tool, ctx, "create", "plain", validTeamGraph).Text)
	tool.Agents = nil
	promote := func(defID any) tools.Result {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+defID.(string)+`"}`))
		return res
	}
	wantRefused(t, promote(created["def_id"]), "promote with no way to check the agents", "cannot check")
	if res := promote(plain["def_id"]); res.IsError {
		t.Errorf("a team with no agents of its own needs no check: %s", res.Text)
	}
}

// A refusal that names a clashing agent tells the caller that agent exists. A
// caller with no agent-authoring grant over the name is not told: the check is
// skipped for it, and the clash is caught when the agent would run.
func TestTeamDefPromote_DoesNotRevealAnAgentTheCallerHasNoGrantOver(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	v1 := decodeResult(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle"}`)).Text)
	v2 := `{"local":{"agents":{}},"states":[{"state":"review","handler":{"kind":"agent","agent":"static-one"}},{"state":"done","handler":{"kind":"terminal"}}]}`
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"sdlc","promote":true,"overlay":`+v2+`}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if res := agentOp(t, tool, ctx, `{"op":"create","name":"sdlc/reviewer","overlay":{"tier":"low"}}`); res.IsError {
		t.Fatalf("seed the clashing agent: %s", res.Text)
	}
	promote := `{"op":"promote","def_id":"` + v1["def_id"].(string) + `"}`
	ungranted := localAuthorCtx("", nil, []string{"Read"})
	if res, _ := tool.Execute(ungranted, json.RawMessage(promote)); res.IsError {
		t.Errorf("a caller with no grant over sdlc/reviewer learned from the refusal that it exists: %s", res.Text)
	}
}
