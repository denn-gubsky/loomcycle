package builtin

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The NEGATIVE cases are the point of this table. A prefix match that let
// `sdlc/*` reach `sdlc/review/sec-review` would silently grant a subtree the
// operator named one level of — so the two wildcards must stay distinguishable.
func TestMatchNamedScope(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// exact, unchanged behaviour
		{"sdlc", "sdlc", true},
		{"sdlc", "sdlc/review", false},
		{"sdlc/review", "sdlc/review", true},

		// one segment
		{"sdlc/*", "sdlc/review", true},
		{"sdlc/*", "sdlc/review/sec-review", false},
		{"sdlc/*", "sdlc", false},
		{"sdlc/review/*", "sdlc/review/sec-review", true},
		{"sdlc/review/*", "sdlc/review", false},

		// all remaining segments
		{"sdlc/**", "sdlc/review", true},
		{"sdlc/**", "sdlc/review/sec-review", true},
		{"sdlc/**", "sdlc", false},

		// a neighbouring prefix must not match — the classic prefix-match bug
		{"sdlc/**", "sdlcx/review", false},
		{"sdlc/*", "sdlcx/review", false},

		// wildcards are positional, not free-floating
		{"*/review", "sdlc/review", true},
		{"*/review", "sdlc/design", false},
		{"*", "sdlc", true},
		{"*", "sdlc/review", false},
		{"**", "sdlc", true},
		{"**", "sdlc/review/sec", true},
	}
	for _, c := range cases {
		if got := matchNamedScope(c.pattern, c.name); got != c.want {
			t.Errorf("matchNamedScope(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// A glob is only reachable through a grant an OPERATOR wrote, and it still
// refuses everything outside it — the gate stays default-deny and a pattern
// cannot be widened into `any` by accident.
func TestCheckScopeForName_GlobGrantsTheSubtreeAndNothingElse(t *testing.T) {
	a := &AgentDef{}
	pol := agentDefPolicy("named:sdlc/**")

	for _, name := range []string{"sdlc/review", "sdlc/review/sec-review"} {
		if err := a.checkScopeForName(pol, name, ""); err != nil {
			t.Errorf("%s: want granted, got %v", name, err)
		}
	}
	for _, name := range []string{"sdlc", "other/review", "sdlcx/review"} {
		if err := a.checkScopeForName(pol, name, ""); err == nil {
			t.Errorf("%s: want refused", name)
		}
	}
}

// Default-deny is unchanged: an agent with no scopes is refused regardless of
// what the name looks like.
func TestCheckScopeForName_NoScopesStillDefaultDeny(t *testing.T) {
	a := &AgentDef{}
	if err := a.checkScopeForName(agentDefPolicy(), "sdlc/review", ""); err == nil {
		t.Error("an agent with no agent_def_scopes must be refused")
	}
}

// agentDefPolicy builds a policy carrying just the scopes under test.
func agentDefPolicy(scopes ...string) tools.AgentDefPolicyValue {
	return tools.AgentDefPolicyValue{Scopes: scopes}
}

// The substrate write path applies the SAME named-pattern rule as operator
// yaml. Without this, an operator who learned the rule from a boot error would
// find the runtime plane quietly keeping a different one — and the bare
// wildcard the yaml refuses would be one AgentDef create away.
func TestAgentDefTool_OverlayNamedScopeValidation(t *testing.T) {
	refused := []string{
		"named:*",           // grants every name — say `any` instead
		"named:**",          //
		"named:sdlc/**/rev", // ** must be last
		"named:sdlc*",       // a wildcard is a whole segment
	}
	for _, sc := range refused {
		if err := validateOverlayNamedScopes([]string{sc}); err == nil {
			t.Errorf("overlay scope %q accepted on the substrate path; yaml refuses it", sc)
		}
	}
	accepted := []string{"named:coder", "named:sdlc/*", "named:sdlc/**", "self", "descendants", "any"}
	for _, sc := range accepted {
		if err := validateOverlayNamedScopes([]string{sc}); err != nil {
			t.Errorf("overlay scope %q refused: %v", sc, err)
		}
	}
	// An unknown non-`named:` string is NOT this function's business — it is
	// default-deny at the gate, and refusing it here would be a separate
	// behaviour change that could break defs already in the wild.
	if err := validateOverlayNamedScopes([]string{"typo-scope"}); err != nil {
		t.Errorf("a non-named scope should pass through untouched, got %v", err)
	}
}
