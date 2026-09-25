package hooks

import (
	"strings"
	"testing"
)

func TestSignDef_TwoDefinitionsThatBehaveTheSameHashTheSame(t *testing.T) {
	a := Def{Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h.example"}}
	b := Def{Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h.example"}, FailMode: FailOpen, Match: &DefMatch{}, Description: "  "}
	if SignDef("gate", a) != SignDef("gate", b) {
		t.Fatalf("defaulted fail_mode / empty match / blank description changed the hash")
	}
	c := a
	c.FailMode = FailClosed
	if SignDef("gate", a) == SignDef("gate", c) {
		t.Fatalf("a different fail_mode hashed the same")
	}
	if SignDef("gate", a) == SignDef("other", a) {
		t.Fatalf("the name is not part of the hash")
	}
	if !strings.HasPrefix(SignDef("gate", a), "sha256:") {
		t.Fatalf("hash %q lacks its prefix", SignDef("gate", a))
	}
}

func TestDefValidate_AcceptsEveryPhaseTheDispatcherRuns(t *testing.T) {
	for _, p := range []Phase{PhasePre, PhasePost, PhasePostFailure, PhaseAgentStart, PhaseAgentStop,
		PhaseSubagentStart, PhaseSubagentStop, PhasePreCompact, PhasePostCompact, PhaseRunEnd} {
		d := Def{Event: p, Body: DefBody{Kind: BodyKindCode, Code: "function hook(ev){return {}}"}}
		if err := d.Validate(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestDefValidate_MatchToolsOnlyOnToolEvents(t *testing.T) {
	d := Def{Event: PhasePost, Match: &DefMatch{Tools: []string{"mcp__jobs__*"}}, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h.example"}}
	if err := d.Validate(); err != nil {
		t.Fatalf("post with tools: %v", err)
	}
	d.Event = PhaseRunEnd
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "match.tools") {
		t.Fatalf("run_end with tools = %v; want refused", err)
	}
}

func TestValidateDefName_RefusesReferenceAndPermitSeparators(t *testing.T) {
	for _, ok := range []string{"gate", "net/deny-internal", "a_b/c-d/E9"} {
		if err := ValidateDefName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "gate@1", "t:gate", "/gate", "gate/", "a//b", "a b", strings.Repeat("x", 129)} {
		if err := ValidateDefName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
