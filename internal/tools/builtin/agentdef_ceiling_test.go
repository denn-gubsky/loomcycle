package builtin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/skillmatch"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// operatorPlane marks ctx the way every operator surface does — the wildcard
// tools ceiling, outside any run — so an authoring call is not narrowed by an
// agent's policies.
func operatorPlane(ctx context.Context) context.Context {
	return tools.WithAgentTools(ctx, []string{"*"})
}

// ceilingFixture is an AgentDef tool with two static volumes and one static
// agent "wide" holding broad capabilities, plus an IN-RUN author: a run id on
// ctx, the tools [Read], and agent_def_scopes [named:sdlc/**] — the reviewer's
// caller. It holds no other policy; each case adds the one it narrows by.
func ceilingFixture(t *testing.T) (*AgentDef, context.Context) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			"sdlc/wide": {
				Tier:         "middle",
				Tools:        []string{"Read"},
				MemoryScopes: []string{"agent", "user", "tenant"},
				Channels:     config.AgentChannelACL{Publish: []string{"ops"}},
				Volumes:      []string{"secrets"},
			},
			// A core block reads its scope whatever memory_scopes say.
			"sdlc/blocks": {
				Tier:       "middle",
				CoreBlocks: []config.CoreBlock{{Label: "brief", Scope: "tenant"}},
			},
		},
		Volumes: map[string]config.Volume{
			"default": {Path: t.TempDir(), Mode: "rw"},
			"work":    {Path: t.TempDir(), Mode: "rw"},
			"secrets": {Path: t.TempDir(), Mode: "rw"},
		},
	}
	tool := &AgentDef{Store: s, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
	ctx := tools.WithRunID(context.Background(), "run_author")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_author", UserID: "u1", TenantID: "t1"})
	ctx = tools.WithAgentName(ctx, "sdlc/meta")
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{Scopes: []string{"named:sdlc/**"}, SelfName: "sdlc/meta"})
	ctx = tools.WithAgentTools(ctx, []string{"Read"})
	// Memory and SQL below their ordinary default (user, tenant), so an unset
	// one passing is that default at work and not the author holding it. The
	// author does hold what an unset history_scope and volumes resolve to
	// ([user], the default volume), so a case leaving those out judges only
	// the field it is about.
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent"}})
	ctx = tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"agent"}})
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: []string{"self", "user"}})
	ctx = tools.WithVolumePolicy(ctx, tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "default"}}})
	return tool, ctx
}

func agentDefOp(t *testing.T, tool *AgentDef, ctx context.Context, op, name, overlay string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"`+name+`","overlay":`+overlay+`}`))
	if err != nil {
		t.Fatalf("%s %q: %v", op, name, err)
	}
	return res
}

// ceilingCase is one capability field: the author policy that holds a narrow
// grant of it, an overlay granting more, and one granting the same or less.
type ceilingCase struct {
	field  string
	policy func(context.Context) context.Context
	wider  string
	within string
	// refusedAs is what the wider value's refusal names, when not the field.
	refusedAs string
	// absentGrantsAll: the field's policy, when no one stamped it, means "all"
	// rather than "none" (skills), so the missing-policy case does not apply.
	absentGrantsAll bool
}

func ceilingCases() []ceilingCase {
	return []ceilingCase{
		{field: "volumes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithVolumePolicy(ctx, tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work"}}})
			},
			wider: `{"volumes":["work","secrets"]}`, within: `{"volumes":["work"]}`},
		{field: "memory_scopes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user"}})
			},
			wider: `{"memory_scopes":["user","tenant"]}`, within: `{"memory_scopes":["user"]}`},
		{field: "memory_consolidation",
			policy: func(ctx context.Context) context.Context {
				return tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent"}, Consolidation: true})
			},
			// The memory grants are judged one by one: holding consolidation
			// is not holding the recall grants.
			wider: `{"memory_consolidation":true,"recall_attach_traces":true}`, refusedAs: "recall_attach_traces:",
			within: `{"memory_consolidation":true}`},
		{field: "core_blocks",
			policy: func(ctx context.Context) context.Context {
				return tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user"}})
			},
			wider:  `{"core_blocks":[{"label":"brief","scope":"tenant"}]}`,
			within: `{"core_blocks":[{"label":"brief","scope":"user"}]}`},
		{field: "sql_scopes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user"}})
			},
			wider: `{"sql_scopes":["user","tenant"]}`, within: `{"sql_scopes":["user"]}`},
		{field: "history_scope",
			policy: func(ctx context.Context) context.Context {
				return tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: []string{"self", "user", "tenant"}})
			},
			// The legacy "any" is the cross-tenant "global" it stands for.
			wider: `{"history_scope":["tenant","any"]}`, within: `{"history_scope":["tenant"]}`},
		{field: "channels.publish",
			policy: func(ctx context.Context) context.Context {
				return tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{Publish: []string{"team/*"}})
			},
			wider: `{"channels":{"publish":["team/*","ops"]}}`, within: `{"channels":{"publish":["team/review","team/x/*"]}}`},
		{field: "channels.subscribe",
			policy: func(ctx context.Context) context.Context {
				return tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{Subscribe: []string{"team/review"}})
			},
			wider: `{"channels":{"subscribe":["team/*"]}}`, within: `{"channels":{"subscribe":["team/review"]}}`},
		{field: "interruption",
			policy: func(ctx context.Context) context.Context {
				return tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{Enabled: true})
			},
			wider: `{"interruption":{"enabled":true,"kinds":["question","approval"]}}`, refusedAs: "interruption.kinds:", within: `{"interruption":{"enabled":true,"max_pending":2}}`},
		{field: "evaluation_scopes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{Scopes: []string{"submit_any"}})
			},
			// submit_any covers every submit; it does not cover reading.
			wider: `{"evaluation_scopes":["submit_self","read_any"]}`, within: `{"evaluation_scopes":["submit_self","submit_descendants"]}`},
		{field: "agent_def_scopes",
			policy: func(ctx context.Context) context.Context { return ctx }, // the fixture's [named:sdlc/**]
			// "self" is the new agent's own name, sdlc/new, inside sdlc/**.
			wider: `{"agent_def_scopes":["any"]}`, within: `{"agent_def_scopes":["named:sdlc/review/*","named:sdlc/x","self"]}`},
		{field: "schedule_def_scopes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{Scopes: []string{"named:nightly"}})
			},
			wider: `{"schedule_def_scopes":["any"]}`, within: `{"schedule_def_scopes":["named:nightly"]}`},
		{field: "a2a_server_card_def_scopes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{Scopes: []string{"named:card"}})
			},
			wider: `{"a2a_server_card_def_scopes":["named:card","named:other"]}`, within: `{"a2a_server_card_def_scopes":["named:card"]}`},
		{field: "a2a_agent_def_scopes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{Scopes: []string{"named:peers/*"}})
			},
			wider: `{"a2a_agent_def_scopes":["named:peers/**"]}`, within: `{"a2a_agent_def_scopes":["named:peers/one"]}`},
		{field: "volume_def_scopes",
			policy: func(ctx context.Context) context.Context {
				return tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{Scopes: []string{"named:scratch"}})
			},
			wider: `{"volume_def_scopes":["any"]}`, within: `{"volume_def_scopes":["named:scratch"]}`},
		{field: "skills",
			policy: func(ctx context.Context) context.Context {
				return tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"doc/*", "-doc/secret"}})
			},
			// Unset is "every skill"; dropping the author's deny widens too.
			wider: `{"system_prompt":"x"}`, within: `{"skills":["doc/redactor","-doc/secret","-doc/old"]}`, absentGrantsAll: true},
	}
}

// An agent inside a run is narrowed by its own policies on every capability
// field: a wider value is refused naming the field, an equal or narrower one
// is stored. The same overlays pass for an operator on the substrate plane.
func TestAgentDefCreate_InRunAuthorIsRefusedEveryCapabilityItDoesNotHold(t *testing.T) {
	for _, c := range ceilingCases() {
		t.Run(c.field, func(t *testing.T) {
			tool, base := ceilingFixture(t)
			ctx := c.policy(base)

			refusedAs := c.refusedAs
			if refusedAs == "" {
				refusedAs = c.field + ":"
			}
			wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/new", c.wider), "wider "+c.field, refusedAs, "ask an operator")
			if res := agentDefOp(t, tool, ctx, "create", "sdlc/new", c.within); res.IsError {
				t.Fatalf("within %s: refused: %s", c.field, res.Text)
			}

			op := operatorPlane(context.Background())
			op = tools.WithAgentDefPolicy(op, tools.AgentDefPolicyValue{Scopes: []string{"any"}})
			if res := agentDefOp(t, tool, op, "create", "sdlc/op-"+strings.ReplaceAll(c.field, ".", "-"), c.wider); res.IsError {
				t.Fatalf("operator plane, wider %s: refused: %s", c.field, res.Text)
			}
		})
	}
}

// A run's author whose ctx carries no policy for a field holds nothing of it:
// even the narrow value is refused, the way the tools ceiling refuses a tools
// overlay when the caller's tools are not on ctx.
func TestAgentDefCreate_MissingAuthorPolicyFailsClosed(t *testing.T) {
	for _, c := range ceilingCases() {
		if c.absentGrantsAll || c.field == "agent_def_scopes" {
			continue // skills: no policy is the documented "all"; agent_def_scopes: the name gate needs one
		}
		t.Run(c.field, func(t *testing.T) {
			tool, ctx := ceilingFixture(t)
			wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/new", c.within), "no policy, "+c.field, c.field+":")
		})
	}
}

// The reviewer's case, verbatim: a caller holding only agent_def_scopes
// [named:sdlc/**] and the tools [Read] created an agent carrying volumes,
// memory_scopes [tenant, global], sql_scopes, agent_def_scopes [any],
// schedule_def_scopes, volume_def_scopes, internal and a channels ACL. It must
// be refused, and nothing stored.
func TestAgentDefCreate_ReviewerEscalationCaseIsRefused(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	res := agentDefOp(t, tool, ctx, "create", "sdlc/escalated", `{"tools":["Read"],
	  "volumes":["secrets"],"memory_scopes":["tenant","global"],"sql_scopes":["tenant"],
	  "agent_def_scopes":["any"],"schedule_def_scopes":["any"],"volume_def_scopes":["any"],
	  "internal":true,"channels":{"publish":["ops"],"subscribe":["ops"]}}`)
	wantRefused(t, res, "the reviewer's escalation")
	if _, err := tool.Store.AgentDefGetActive(context.Background(), "t1", "sdlc/escalated"); err == nil {
		t.Fatal("the refused definition was stored")
	}
}

// The operator plane is told apart by the wildcard tools ceiling outside a run.
// The same wildcard on a run's ctx grants nothing: a run is always narrowed.
func TestAgentDefCreate_WildcardToolsInsideARunIsStillNarrowed(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	ctx = tools.WithAgentTools(ctx, []string{"*"})
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/new", `{"sql_scopes":["tenant"]}`), "wildcard in a run", "sql_scopes:")

	off := operatorPlane(context.Background())
	off = tools.WithAgentDefPolicy(off, tools.AgentDefPolicyValue{Scopes: []string{"named:sdlc/**"}})
	if res := agentDefOp(t, tool, off, "create", "sdlc/new", `{"sql_scopes":["tenant"],"volumes":["secrets"]}`); res.IsError {
		t.Fatalf("operator plane refused: %s", res.Text)
	}
}

// Only the memory facets fall back unjudged when unset: memory_scopes and
// sql_scopes take the ordinary memory default though the author holds less,
// and an unset evaluation_scopes is submit_self. Every other unset field is
// judged at what it resolves to — the operator's default volume (a host
// directory), the user's chats, the question interruption kind, every skill —
// and the refusal says how to narrow it.
func TestAgentDefCreate_UnsetFieldIsJudgedUnlessItIsAMemoryDefault(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	// Below every default: memory and SQL [agent] (the fixture), history
	// [self], bound to the volume "work" only, interruptions of kind approval.
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: []string{"self"}})
	ctx = tools.WithVolumePolicy(ctx, tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work"}}})
	ctx = tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{Enabled: true, Kinds: []string{"approval"}})
	const rest = `"history_scope":["self"],"volumes":["work"]`

	if res := agentDefOp(t, tool, ctx, "create", "sdlc/new", `{`+rest+`}`); res.IsError {
		t.Fatalf("unset memory_scopes / sql_scopes / evaluation_scopes were judged: %s", res.Text)
	}
	if res := agentDefOp(t, tool, ctx, "create", "sdlc/denied", `{"memory_scopes":["-*"],"sql_scopes":["-*"],"history_scope":["-*"],"volumes":["work"]}`); res.IsError {
		t.Fatalf("an explicit deny-all must always pass: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/x", `{"memory_scopes":["user"],`+rest+`}`), "explicit memory_scopes", "memory_scopes:")

	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/x", `{"volumes":["work"]}`),
		"unset history_scope", "history_scope: left unset", `"user"`, "set history_scope explicitly", `["-*"]`)
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/x", `{"history_scope":["self"]}`),
		"unset volumes", "volumes: left unset", `"default"`, "set volumes explicitly")
	// The escalation the default would hand out: an author with no volume at
	// all minting an agent bound to the operator's default host directory.
	noVolumes := tools.WithVolumePolicy(ctx, tools.VolumePolicyValue{})
	wantRefused(t, agentDefOp(t, tool, noVolumes, "create", "sdlc/x", `{"history_scope":["self"]}`),
		"unset volumes, author holds none", "volumes: left unset", `"default"`)
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/x", `{`+rest+`,"interruption":{"enabled":true}}`),
		"unset interruption kinds", "interruption.kinds: left unset", `"question"`)
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/x", `{`+rest+`,"interruption":{"enabled":true,"kinds":["question"]}}`),
		"explicit kinds", "interruption.kinds:")

	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"doc/*"}})
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/x", `{`+rest+`}`), "unset skills", "skills: left unset")
}

// A volume the author holds read-only (a sub-agent of a read-only parent) is
// declared read-write, so a new agent naming it could write: refused.
func TestAgentDefCreate_ReadOnlyHeldVolumeIsNotGrantedReadWrite(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	ctx = tools.WithVolumePolicy(ctx, tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work", ReadOnly: true}}})
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/new", `{"volumes":["work"]}`), "ro volume", "read-only")
}

// A fork keeps what the version it forks holds — rewriting only the prompt of
// a wide agent is not a widening — may narrow it, and may not add anything
// neither the forker nor that version holds.
func TestAgentDefFork_CapabilityCeilingKeepsTheLineageButCannotWiden(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}})

	if res := agentDefOp(t, tool, ctx, "fork", "sdlc/wide", `{"system_prompt":"rewritten"}`); res.IsError {
		t.Fatalf("prompt-only fork refused: %s", res.Text)
	}
	if res := agentDefOp(t, tool, ctx, "fork", "sdlc/wide", `{"system_prompt":"keeps all","sql_scopes":["agent"],"history_scope":["self"]}`); res.IsError {
		t.Fatalf("fork inheriting the parent's memory_scopes, channels and volumes refused: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "fork", "sdlc/wide", `{"channels":{"publish":["ops","other"]}}`),
		"fork adding a channel", "channels.publish:", `"other"`, "forked version")
	wantRefused(t, agentDefOp(t, tool, ctx, "fork", "sdlc/wide", `{"volumes":["secrets","work"]}`),
		"fork adding a volume", "volumes:", `"work"`)
	wantRefused(t, agentDefOp(t, tool, ctx, "fork", "sdlc/wide", `{"sql_scopes":["tenant"],"history_scope":["self"]}`),
		"fork adding an SQL scope", "sql_scopes:")
	if res := agentDefOp(t, tool, ctx, "fork", "sdlc/blocks", `{"system_prompt":"rewritten","sql_scopes":["agent"],"history_scope":["self"]}`); res.IsError {
		t.Fatalf("prompt-only fork keeping the parent's tenant core block refused: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "fork", "sdlc/blocks", `{"sql_scopes":["agent"],"history_scope":["self"],"core_blocks":[{"label":"brief","scope":"tenant"},{"label":"x","scope":"agent"}]}`),
		"fork adding a core block", "core_blocks:", `"x"`)
}

// A team's own agent passes gateNewDef, so the ceiling holds for it too.
func TestTeamDefCreate_LocalAgentIsHeldToTheCapabilityCeiling(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	ctx = tools.WithRunID(ctx, "run_author")
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle","tools":["Read"],"sql_scopes":["tenant"]}`)),
		"team-local agent with sql_scopes", "local.agents", "sql_scopes:")
	ctx = tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	if res := teamOp(t, tool, ctx, "create", "sdlc", localTeam(`{"tier":"middle","tools":["Read"],"sql_scopes":["tenant"]}`)); res.IsError {
		t.Fatalf("team-local agent within the author's sql_scopes refused: %s", res.Text)
	}
}

func TestSkillsWithin_IsNarrowOnly(t *testing.T) {
	for _, c := range []struct {
		child, ceiling []string
		want           bool
	}{
		{nil, nil, true},
		{[]string{"*"}, nil, true},
		{nil, []string{"doc/*"}, false},
		{[]string{"-*"}, []string{"doc/*"}, true},
		{[]string{"doc/a"}, []string{"doc/*"}, true},
		{[]string{"doc/*"}, []string{"doc/*"}, true},
		{[]string{"doc/a/b"}, []string{"doc/*"}, false},
		{[]string{"doc/a/*"}, []string{"doc/**"}, true},
		{[]string{"doc/**"}, []string{"doc/*"}, false},
		{[]string{"other"}, []string{"doc/*"}, false},
		{[]string{"doc/a"}, []string{"doc/*", "-doc/a"}, false},
		{[]string{"doc/a", "-doc/a"}, []string{"doc/*", "-doc/a"}, true},
		{[]string{"-x"}, []string{"-x"}, true},
		{nil, []string{"-x"}, false},
		{[]string{"doc/*", "-x"}, []string{"-x"}, true},
		{[]string{"./local"}, []string{"doc/*"}, true},
		{[]string{"./local"}, []string{"-x"}, true},
		{[]string{"./local", "other"}, []string{"-x"}, false},
		// A deny-all author grants nothing, not even the team's own skills.
		{[]string{"./local"}, []string{"-*"}, false},
		// A mixed list is judged on its global entries.
		{[]string{"./local", "*"}, []string{"doc/*"}, false},
		{[]string{"./local", "*"}, []string{"-x"}, false},
		// Not one well-formed local name: judged as a pattern.
		{[]string{"./*"}, []string{"doc/*"}, false},
		{[]string{"./a/b"}, []string{"doc/*"}, false},
		{[]string{"anything"}, []string{"-*"}, false},
	} {
		if got := skillsWithin(c.child, c.ceiling); got != c.want {
			t.Errorf("skillsWithin(%v, %v) = %v, want %v", c.child, c.ceiling, got, c.want)
		}
	}
}

func TestDefScopeCovered_IsNarrowOnly(t *testing.T) {
	for _, c := range []struct {
		scope, child string
		ceiling      []string
		self         string
		want         bool
	}{
		{"any", "x", []string{"any"}, "", true},
		{"any", "x", []string{"named:x"}, "", false},
		{"descendants", "x", []string{"named:x"}, "", false},
		{"named:sdlc/a", "x", []string{"named:sdlc/*"}, "", true},
		{"named:sdlc/*", "x", []string{"named:sdlc/*"}, "", true},
		{"named:sdlc/**", "x", []string{"named:sdlc/*"}, "", false},
		{"named:sdlc/a/*", "x", []string{"named:sdlc/**"}, "", true},
		{"named:sdlc", "x", []string{"named:sdlc/**"}, "", false},
		{"self", "sdlc/new", []string{"named:sdlc/**"}, "", true},
		{"self", "other", []string{"named:sdlc/**"}, "", false},
		{"named:meta", "x", []string{"self"}, "meta", true},
		{"self", "x", []string{"self"}, "meta", false},
		{"bogus", "x", []string{"named:x"}, "", false},
		{"bogus", "x", []string{"bogus"}, "", true},
	} {
		if got := defScopeCovered(c.scope, c.child, c.ceiling, c.self); got != c.want {
			t.Errorf("defScopeCovered(%q, child %q, %v, self %q) = %v, want %v", c.scope, c.child, c.ceiling, c.self, got, c.want)
		}
	}
}

// A team's own agent may be granted the team's own channels and skills by
// "./<name>": the author holds those by authoring the team, so they are not
// judged against its channel ACL or skills allowlist. Its other entries are.
func TestTeamDefCreate_LocalAgentGrantsOfTheTeamsOwnChannelsAndSkillsAreNotJudged(t *testing.T) {
	tool, ctx := skillTeamFixture(t)
	ctx = tools.WithRunID(ctx, "run_author")
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{Publish: []string{"team/*"}})
	// The team's skill is authored as "<team>/style", which this allows; it
	// does not allow "./style" as a pattern.
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"sdlc/*", "sdlc2/*"}})
	team := func(publish string) string {
		return `{"entry":"review","local":{"channels":{"events":{"scope":"tenant"}},
		  "agents":{"reviewer":{"tier":"middle","tools":["Read","Skill"],"skills":["./style"],
		    "channels":{"publish":` + publish + `,"subscribe":["./events"]}}},
		  "skills":{"style":{"body":"Prefer short functions.","tools":["Read"]}}},
		  "states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},
		            {"state":"done","handler":{"kind":"terminal"}}],
		  "transitions":[{"from":"review","to":"done","on":"success"}]}`
	}
	if res := teamOp(t, tool, ctx, "create", "sdlc", team(`["./events","team/x"]`)); res.IsError {
		t.Fatalf("grants of the team's own channel and skill were judged: %s", res.Text)
	}
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc2", team(`["./events","ops"]`)),
		"a global channel beside the team's", `local.agents["reviewer"]`, "channels.publish:", `"ops"`)
}

// Only "./" and one well-formed local name is a team's own resource. Every
// other "./" spelling is judged like any entry, so none slips past the ceiling
// as "local" while the channel matcher would read it as a name or pattern.
func TestIsTeamLocalRef_AcceptsOnlyOneWellFormedName(t *testing.T) {
	for entry, want := range map[string]bool{
		"./events": true, " ./a-b_1 ": true,
		"./": false, "./*": false, "./a/b": false, "./../x": false, "./a.b": false,
		"./" + strings.Repeat("x", 65): false, "events": false, ".//x": false, "./a/*": false,
	} {
		if got := isTeamLocalRef(entry); got != want {
			t.Errorf("isTeamLocalRef(%q) = %v, want %v", entry, got, want)
		}
	}
}

// An in-run author whose channel ACL is [team/*] cannot pass a malformed "./"
// entry as one of a team's own channels.
func TestAgentDefCreate_MalformedLocalChannelRefIsJudged(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{Publish: []string{"team/*"}})
	for _, entry := range []string{"./*", "./a/b", "./../x", "./"} {
		wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/new", `{"channels":{"publish":[`+strconv.Quote(entry)+`]}}`),
			"publish "+entry, "channels.publish:")
	}
}

// A global agent granted only "./x" skills can invoke no global skill, so
// passing the ceiling on such a list hands out nothing beyond the team.
func TestAgentDefCreate_OnlyLocalSkillRefsGrantNoGlobalSkill(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"doc/*"}})
	res := agentDefOp(t, tool, ctx, "create", "sdlc/new", `{"skills":["./x"]}`)
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	row, err := tool.Store.AgentDefGetActive(context.Background(), "t1", "sdlc/new")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var def mergedDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	for _, global := range []string{"doc/redactor", "x", "other", "a/b/c"} {
		if skillmatch.Allowed(def.Skills, global) {
			t.Errorf("an agent granted %v may invoke the global skill %q", def.Skills, global)
		}
	}
	denyAll := tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"-*"}})
	wantRefused(t, agentDefOp(t, tool, denyAll, "create", "sdlc/new2", `{"skills":["./x"]}`), "deny-all author", "skills:")
}
