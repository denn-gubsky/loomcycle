package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

type namedTool struct{ name string }

func (n namedTool) Name() string                                             { return n.name }
func (n namedTool) Description() string                                      { return "" }
func (n namedTool) InputSchema() json.RawMessage                             { return json.RawMessage(`{}`) }
func (n namedTool) Execute(context.Context, json.RawMessage) (Result, error) { return Result{}, nil }

func compiledSet(t *testing.T, rules ...authz.Rule) *authz.Effective {
	t.Helper()
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "t", ID: "u"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

func modeCtx(m authz.Mode) context.Context { return authz.WithMode(context.Background(), m) }

func names(ts []Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestNarrowToolsByACL_PerMode(t *testing.T) {
	in := []Tool{namedTool{"Read"}, namedTool{"Bash"}, namedTool{"mcp__jobs__patch"}, namedTool{"mcp__jobs__get"}}
	eff := compiledSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"tool:Read", "mcp:jobs/get"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash", "mcp:jobs/patch*"}})
	kept, removed := NarrowToolsByACL(modeCtx(authz.ModeCompat), eff, in)
	if !reflect.DeepEqual(names(kept), []string{"Read", "mcp__jobs__get"}) || !reflect.DeepEqual(removed, []string{"Bash", "mcp__jobs__patch"}) {
		t.Errorf("compat: kept %v removed %v", names(kept), removed)
	}
	// Enforce keeps only what is allowed; a tool no rule names goes too.
	in2 := append(in, namedTool{"Write"})
	kept, removed = NarrowToolsByACL(modeCtx(authz.ModeEnforce), eff, in2)
	if !reflect.DeepEqual(names(kept), []string{"Read", "mcp__jobs__get"}) || !reflect.DeepEqual(removed, []string{"Bash", "mcp__jobs__patch", "Write"}) {
		t.Errorf("enforce: kept %v removed %v", names(kept), removed)
	}
	// Shadow removes nothing; no set in compat removes nothing; no set in
	// enforce removes everything.
	if kept, removed := NarrowToolsByACL(modeCtx(authz.ModeShadow), eff, in); len(kept) != len(in) || removed != nil {
		t.Errorf("shadow: kept %v removed %v", names(kept), removed)
	}
	if kept, removed := NarrowToolsByACL(modeCtx(authz.ModeCompat), nil, in); len(kept) != len(in) || removed != nil {
		t.Errorf("compat/no set: kept %v removed %v", names(kept), removed)
	}
	if kept, _ := NarrowToolsByACL(modeCtx(authz.ModeEnforce), nil, in); len(kept) != 0 {
		t.Errorf("enforce/no set: kept %v", names(kept))
	}
}

func TestNarrowVolumesByACL(t *testing.T) {
	v := VolumePolicyValue{Active: true, Bindings: []VolumeBinding{
		{Name: "scratch", Root: "/s"}, {Name: "shared-ro", Root: "/r", ReadOnly: true}, {Name: "secrets", Root: "/x"},
	}}
	eff := compiledSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"volume:scratch"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read", "write"}, Resources: []string{"volume:secrets"}})
	got := NarrowVolumesByACL(modeCtx(authz.ModeCompat), eff, v)
	if len(got.Bindings) != 2 || got.Bindings[0].Name != "scratch" || !got.Bindings[0].ReadOnly || got.Bindings[1].Name != "shared-ro" {
		t.Errorf("compat: %+v", got.Bindings)
	}
	// Enforce: only an allowed binding survives; write needs an allow.
	eff2 := compiledSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"volume:scratch", "volume:shared-ro"}})
	got = NarrowVolumesByACL(modeCtx(authz.ModeEnforce), eff2, v)
	if len(got.Bindings) != 2 || !got.Bindings[0].ReadOnly || got.Bindings[0].Name != "scratch" {
		t.Errorf("enforce: %+v", got.Bindings)
	}
	// An inactive policy and a no-set compat run are untouched.
	if got := NarrowVolumesByACL(modeCtx(authz.ModeEnforce), nil, VolumePolicyValue{}); got.Active {
		t.Error("inactive")
	}
	if got := NarrowVolumesByACL(modeCtx(authz.ModeCompat), nil, v); len(got.Bindings) != 3 {
		t.Error("no set in compat")
	}
}

func TestNarrowScopesByACL(t *testing.T) {
	eff := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"memory:tenant", "history:tenant"}})
	if got := NarrowScopesByACL(modeCtx(authz.ModeCompat), eff, "memory", []string{"user", "tenant", "agent"}); !reflect.DeepEqual(got, []string{"user", "agent"}) {
		t.Errorf("memory compat = %v", got)
	}
	if got := NarrowScopesByACL(modeCtx(authz.ModeCompat), eff, "history", []string{"self", "user", "tenant"}); !reflect.DeepEqual(got, []string{"self", "user"}) {
		t.Errorf("history compat = %v", got)
	}
	allowUser := compiledSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"sqlmem:user"}})
	if got := NarrowScopesByACL(modeCtx(authz.ModeEnforce), allowUser, "sqlmem", []string{"user", "tenant"}); !reflect.DeepEqual(got, []string{"user"}) {
		t.Errorf("sqlmem enforce = %v", got)
	}
	if got := NarrowScopesByACL(modeCtx(authz.ModeCompat), nil, "memory", []string{"user"}); !reflect.DeepEqual(got, []string{"user"}) {
		t.Errorf("no set = %v", got)
	}
}

func TestNarrowSkillsByACL(t *testing.T) {
	eff := compiledSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"skill:support/**", "skill:seo"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"skill:marketing/*"}, When: &authz.Condition{Transport: []string{"grpc"}}})
	sp := SkillPolicyValue{Patterns: []string{"doc/*"}, Granted: true}
	got := NarrowSkillsByACL(modeCtx(authz.ModeCompat), eff, sp)
	// The conditional deny is not a pattern; the two unconditional ones are.
	if !reflect.DeepEqual(got.Patterns, []string{"doc/*", "-support/**", "-seo"}) || !got.Granted {
		t.Errorf("compat = %+v", got)
	}
	// Enforce with no skill allow at all: every skill denied.
	got = NarrowSkillsByACL(modeCtx(authz.ModeEnforce), eff, sp)
	if !reflect.DeepEqual(got.Patterns, []string{"doc/*", "-*"}) {
		t.Errorf("enforce without allow = %+v", got)
	}
	allow := compiledSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"skill:doc/*"}}, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"skill:*"}})
	if got := NarrowSkillsByACL(modeCtx(authz.ModeCompat), allow, sp); !reflect.DeepEqual(got.Patterns, []string{"doc/*", "-*"}) {
		t.Errorf("deny-all pattern = %+v", got)
	}
	if got := NarrowSkillsByACL(modeCtx(authz.ModeEnforce), nil, sp); !reflect.DeepEqual(got.Patterns, []string{"-*"}) {
		t.Errorf("enforce no set = %+v", got)
	}
	if got := NarrowSkillsByACL(modeCtx(authz.ModeCompat), nil, sp); !reflect.DeepEqual(got, sp) {
		t.Errorf("compat no set = %+v", got)
	}
}

// shadowPolicy is a run policy as shadow mode leaves it: every grant of the
// definition, nothing narrowed.
func shadowPolicy(eff *authz.Effective) RunPolicy {
	return RunPolicy{
		Tools: []string{"Read", "Bash", "Write"},
		Volumes: VolumePolicyValue{Active: true, Bindings: []VolumeBinding{
			{Name: "scratch", Root: "/s"}, {Name: "shared", Root: "/h"}, {Name: "secrets", Root: "/x"}, {Name: "docs", Root: "/d", ReadOnly: true},
		}},
		Memory:  MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}},
		SqlMem:  SqlMemPolicyValue{AllowedScopes: []string{"user"}},
		History: HistoryPolicyValue{Scopes: []string{"self"}},
		Skill:   SkillPolicyValue{Granted: true},
		ACL:     eff,
	}
}

func divergenceKeys(ds []ACLShadowDivergence) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Surface.Action+" "+d.Surface.Type+":"+d.Surface.Name+" "+d.Decision.Outcome.String())
	}
	return out
}

// In shadow mode the run keeps what the narrowing helpers would take in
// enforce; ShadowACLNarrowing names each of those, with the engine's answer
// — a rule's deny and a question no rule allows alike, since enforce takes
// both.
func TestShadowACLNarrowing_NamesWhatEnforceWouldTake(t *testing.T) {
	eff := compiledSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"tool:Read"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}},
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"volume:scratch"}},
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"volume:shared", "volume:docs", "memory:user", "sqlmem:user", "history:self"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read", "write"}, Resources: []string{"volume:secrets"}})
	got := divergenceKeys(ShadowACLNarrowing(modeCtx(authz.ModeShadow), shadowPolicy(eff)))
	want := []string{
		"invoke tool:Bash deny",
		"invoke tool:Write no_match",
		"write volume:shared no_match",
		"read volume:secrets deny",
		"read memory:tenant no_match",
		"invoke skill:* no_match",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shadow divergences:\n got  %q\n want %q", got, want)
	}
	// The deciding rules travel with each one.
	if ds := ShadowACLNarrowing(modeCtx(authz.ModeShadow), shadowPolicy(eff)); len(ds) == 0 || len(ds[0].Decision.Reasons) == 0 || ds[0].Decision.Reasons[0].ACL != "a" {
		t.Errorf("the deny carries no deciding rule: %+v", ds)
	}
}

// Compat and enforce narrowed the run themselves, and a run with no set has
// nothing to be compared with: none of them reports anything.
func TestShadowACLNarrowing_NothingOutsideShadowOrWithoutASet(t *testing.T) {
	eff := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}})
	for _, m := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		if got := ShadowACLNarrowing(modeCtx(m), shadowPolicy(eff)); got != nil {
			t.Errorf("%s: %q", m, divergenceKeys(got))
		}
	}
	if got := ShadowACLNarrowing(modeCtx(authz.ModeShadow), shadowPolicy(nil)); got != nil {
		t.Errorf("shadow with no set: %q", divergenceKeys(got))
	}
	// A set that allows everything the run holds: nothing to report.
	all := compiledSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"tool:*", "skill:*"}},
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"volume:*", "memory:*", "sqlmem:*"}},
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"history:*"}})
	if got := ShadowACLNarrowing(modeCtx(authz.ModeShadow), shadowPolicy(all)); got != nil {
		t.Errorf("shadow, all allowed: %q", divergenceKeys(got))
	}
}
