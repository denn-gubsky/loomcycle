package hooks

import (
	"context"
	"reflect"
	"testing"
)

func mustRegister(t *testing.T, s *Set, h *Hook) string {
	t.Helper()
	id, err := s.Register(h)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return id
}

func hookNames(hs []*Hook) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name
	}
	return out
}

func TestSet_MatchKeepsListedOrderAndReversesPost(t *testing.T) {
	s := NewSet()
	for _, n := range []string{"a", "b", "c"} {
		mustRegister(t, s, &Hook{Owner: "agent:w", Name: n, Phase: PhasePre, CallbackURL: "https://h.example/" + n})
		mustRegister(t, s, &Hook{Owner: "agent:w", Name: n, Phase: PhasePost, CallbackURL: "https://h.example/" + n})
	}
	if got := hookNames(s.Match("w", "Read", PhasePre)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("pre = %v", got)
	}
	if got := hookNames(s.Match("w", "Read", PhasePost)); !reflect.DeepEqual(got, []string{"c", "b", "a"}) {
		t.Fatalf("post = %v", got)
	}
}

// A run may add hooks but never merge them: the same hook listed twice runs
// twice, where the old registry replaced the first.
func TestSet_AHookListedTwiceRunsTwice(t *testing.T) {
	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: "https://h.example"})
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: "https://h.example"})
	if n := len(s.Match("w", "Read", PhasePre)); n != 2 {
		t.Fatalf("matched %d; want 2", n)
	}
}

func TestPermits_KeyedOnTenantAndName(t *testing.T) {
	p := NewPermits([]string{"jobember:url-gate", " op-gate ", "tenant:", ""})
	if !p.Has("jobember", "url-gate") || !p.Has("", "op-gate") {
		t.Fatalf("permits lost an entry")
	}
	if p.Has("other", "url-gate") || p.Has("", "url-gate") || p.Has("tenant", "") {
		t.Fatalf("permit widened beyond its (tenant, name)")
	}
}

func fakeLookup(defs map[string]Def) LookupDef {
	return func(_ context.Context, tenant, name string, version int) (Def, string, error) {
		d, ok := defs[tenant+"|"+name]
		if !ok {
			return Def{}, "", errNotFoundForTest
		}
		return d, "hdf_" + tenant + "_" + name, nil
	}
}

var errNotFoundForTest = &wrappedError{sentinel: ErrInvalidRegistration, msg: "not found"}

func TestResolve_ToolHooksFirstThenAgentHooks(t *testing.T) {
	defs := map[string]Def{
		"acme|deny-internal": {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h.example/d"}, FailMode: FailClosed},
		"|redact":            {Event: PhasePost, Body: DefBody{Kind: BodyKindCode, Code: "function hook(ev){}"}},
	}
	tools := ToolHooks{"WebFetch": {PhasePre: {{Ref: "deny-internal"}, {Inline: &Inline{Name: "url-gate", URL: "https://app.example/g"}}}}}
	agent := EventHooks{PhasePre: {{Inline: &Inline{Name: "audit", URL: "https://app.example/a"}}}, PhasePost: {{Ref: "redact"}}}
	s := NewSet()
	src := Source{Owner: "agent:researcher", Tenant: "acme", OperatorAuthored: true}
	if err := Resolve(context.Background(), src, agent, tools, fakeLookup(defs), NewPermits([]string{"acme:url-gate"}), s); err != nil {
		t.Fatal(err)
	}
	pre := s.Match("researcher", "WebFetch", PhasePre)
	if got := hookNames(pre); !reflect.DeepEqual(got, []string{"deny-internal", "url-gate", "audit"}) {
		t.Fatalf("WebFetch pre = %v; want the tool's own hooks, then the agent's", got)
	}
	if got := hookNames(s.Match("researcher", "Read", PhasePre)); !reflect.DeepEqual(got, []string{"audit"}) {
		t.Fatalf("Read pre = %v; a tool's hooks must not reach another tool", got)
	}
	if pre[0].DefID != "hdf_acme_deny-internal" || pre[0].FailMode != FailClosed || pre[0].Tenant != "acme" {
		t.Fatalf("resolved ref = %+v", pre[0])
	}
	if !pre[1].WidenPermitted || pre[0].WidenPermitted || pre[2].WidenPermitted {
		t.Fatalf("widen = %v %v %v; only the permitted url-gate may widen", pre[0].WidenPermitted, pre[1].WidenPermitted, pre[2].WidenPermitted)
	}
	post := s.Match("researcher", "Read", PhasePost)
	if len(post) != 1 || post[0].Code == "" || post[0].Tenant != "" {
		t.Fatalf("shared ref resolved as %+v; want the shared code hook, tenant \"\"", post)
	}
}

func TestResolve_RefusesWhatWouldSilentlyOpenAGate(t *testing.T) {
	defs := map[string]Def{
		"|stop-check": {Event: PhaseAgentStop, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h.example"}},
		"|web-only":   {Event: PhasePre, Match: &DefMatch{Tools: []string{"WebFetch"}}, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h.example"}},
	}
	cases := map[string]struct {
		agent EventHooks
		tools ToolHooks
	}{
		"missing HookDef":         {agent: EventHooks{PhasePre: {{Ref: "gone"}}}},
		"event mismatch":          {agent: EventHooks{PhasePre: {{Ref: "stop-check"}}}},
		"match excludes the tool": {tools: ToolHooks{"Read": {PhasePre: {{Ref: "web-only"}}}}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := Resolve(context.Background(), Source{Owner: "agent:w"}, c.agent, c.tools, fakeLookup(defs), Permits{}, NewSet())
			if err == nil {
				t.Fatalf("resolved; want refused")
			}
		})
	}
}

func TestResolve_NoWideningWithoutAnOperatorAuthoredDefinition(t *testing.T) {
	agent := EventHooks{PhasePre: {{Inline: &Inline{Name: "url-gate", URL: "https://app.example/g"}}}}
	s := NewSet()
	if err := Resolve(context.Background(), Source{Owner: "agent:w", Tenant: "acme"}, agent, nil, nil, NewPermits([]string{"acme:url-gate"}), s); err != nil {
		t.Fatal(err)
	}
	if s.List()[0].WidenPermitted {
		t.Fatalf("a hook from an agent-authored definition was allowed to widen")
	}
}

func TestEventHooks_ValidateRefusesRunEventsUnderATool(t *testing.T) {
	if err := (ToolHooks{"Read": {PhaseAgentStop: {{Ref: "x"}}}}).Validate(); err == nil {
		t.Fatalf("agent_stop accepted under a tool")
	}
	if err := (EventHooks{"pre_tool_use": {{Ref: "x"}}}).Validate(""); err == nil {
		t.Fatalf("unknown event accepted")
	}
	if err := (EventHooks{PhasePre: {{Inline: &Inline{Name: "g", URL: "ftp://x"}}}}).Validate(""); err == nil {
		t.Fatalf("a non-http inline url accepted")
	}
	if err := (EventHooks{PhasePre: {{Ref: "gate@0"}}}).Validate(""); err == nil {
		t.Fatalf("version 0 accepted")
	}
	if err := (EventHooks{PhaseAgentStop: {{Ref: "cite@3"}}, PhasePre: {{Inline: &Inline{Name: "g", URL: "https://x"}}}}).Validate(""); err != nil {
		t.Fatalf("valid hooks refused: %v", err)
	}
}

func TestAdditions_MergeAppendsAndValidateChecksTheAgentsTools(t *testing.T) {
	a := Additions{Hooks: EventHooks{PhaseRunEnd: {{Ref: "a"}}}}
	b := Additions{Hooks: EventHooks{PhaseRunEnd: {{Ref: "b"}}}, ToolHooks: ToolHooks{"Read": {PhasePre: {{Ref: "c"}}}}}
	m := a.Merge(b)
	if got := m.Hooks[PhaseRunEnd]; len(got) != 2 || got[0].Ref != "a" || got[1].Ref != "b" {
		t.Fatalf("merged run_end = %v; want a then b", got)
	}
	if len(a.Hooks[PhaseRunEnd]) != 1 {
		t.Fatalf("Merge changed its receiver")
	}
	if !(Additions{}).Empty() || m.Empty() {
		t.Fatalf("Empty is wrong")
	}
	if err := b.Validate([]string{"Read"}); err != nil {
		t.Fatalf("valid additions refused: %v", err)
	}
	if err := b.Validate([]string{"Write"}); err == nil {
		t.Fatalf("a hook on a tool the agent lacks was accepted")
	}
}
