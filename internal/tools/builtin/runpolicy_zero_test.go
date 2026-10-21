package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run entry point that forgets to build a RunPolicy fails CLOSED: its ctx
// carries a run identity (a run is plainly running) and nothing else, and every
// policy-gated tool refuses. Each call below is one the same tool allows once
// the matching grant is on the policy — so the refusals are the missing policy,
// not a broken fixture.
func TestNoRunPolicy_EveryGrantGatedToolRefuses(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	set := loadSetWithSkills(t, []struct {
		Name  string
		Tools []string
		Body  string
	}{{Name: "doc/guide", Tools: []string{"Skill"}, Body: "GUIDE"}})
	cfg := &config.Config{}

	bare := tools.WithRunID(tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserID: "u1", AgentID: "a_new_entry", RootRunID: "r_new_entry",
	}), "r_new_entry")
	bare = tools.WithAgentName(bare, "newcomer")

	granted := tools.WithRunPolicy(bare, tools.RunPolicy{
		Tools:      []string{"Skill", "Memory", "AgentDef", "Interruption", "Evaluation"},
		Skill:      tools.SkillPolicyValue{Granted: true},
		Memory:     tools.MemoryPolicyValue{AllowedScopes: []string{"user"}},
		AgentDef:   tools.AgentDefPolicyValue{Scopes: []string{"any"}, SelfName: "newcomer"},
		Evaluation: tools.EvaluationPolicyValue{Scopes: []string{"submit_self"}},
	})

	cases := []struct {
		name  string
		tool  tools.Tool
		input string
		// allowedToo: the same call succeeds under the matching grant. Some
		// tools need more than a grant to succeed (a pending interrupt, a
		// real run row) — for those only the refusal is asserted.
		allowedToo bool
	}{
		{"Skill invoke", &SkillTool{Set: set}, `{"name":"doc/guide"}`, true},
		{"Memory set", &Memory{Store: st}, `{"op":"set","scope":"user","key":"k","value":"v"}`, true},
		{"AgentDef create", &AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 1 << 17, MaxDescriptionBytes: 8192},
			`{"op":"create","name":"minted","overlay":{"system_prompt":"x","history_scope":["-*"]}}`, true},
		{"Interruption ask", &Interruption{Store: st}, `{"op":"ask","question":"ok?"}`, false},
		{"Evaluation submit", &Evaluation{Store: st}, `{"op":"submit","score":1}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := c.tool.Execute(bare, json.RawMessage(c.input))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !res.IsError {
				t.Errorf("with no run policy on ctx the call succeeded: %s", res.Text)
			}
			if !c.allowedToo {
				return
			}
			res, err = c.tool.Execute(granted, json.RawMessage(c.input))
			if err != nil {
				t.Fatalf("Execute (granted): %v", err)
			}
			if res.IsError {
				t.Errorf("fixture: the call is refused under its grant too, so the refusal above proves nothing: %s", res.Text)
			}
		})
	}
}

// The zero RunPolicy — what a run whose entry point built none reads — grants
// nothing. Narrowings of an operator floor (hosts, decision models, fan-out)
// are not grants and are not listed.
func TestNoRunPolicy_ZeroPolicyGrantsNothing(t *testing.T) {
	p := tools.PolicyOf(context.Background())
	if p.Tools != nil || p.ToolPatterns != nil {
		t.Error("a tools ceiling")
	}
	if p.Volumes.Active || p.EphemeralWritable || len(p.VolumeDef.Scopes) > 0 {
		t.Error("filesystem access")
	}
	if len(p.Memory.AllowedScopes) > 0 || p.Memory.Consolidation || len(p.SqlMem.AllowedScopes) > 0 || len(p.History.Scopes) > 0 {
		t.Error("memory, SQL or history access")
	}
	if p.Channel.AllPublish || p.Channel.AllSubscribe || len(p.Channel.Publish) > 0 || len(p.Channel.Subscribe) > 0 {
		t.Error("channel access")
	}
	if p.Interruption.Load().Enabled {
		t.Error("interruptions")
	}
	if len(p.AgentDef.Scopes) > 0 || len(p.Evaluation.Scopes) > 0 || len(p.Crypto.Scopes) > 0 || p.OperatorAuthored {
		t.Error("def-family, evaluation or key scopes, or operator authorship")
	}
	if eff := p.Skill.Effective(); len(eff) != 1 || eff[0] != "-*" {
		t.Errorf("skills: the zero policy evaluates as %v, want deny-all", eff)
	}
}
