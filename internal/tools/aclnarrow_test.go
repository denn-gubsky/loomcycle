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
