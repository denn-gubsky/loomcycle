package auth

import (
	"strings"
	"testing"
)

func TestRestrictions_ZeroRestrictsNothing(t *testing.T) {
	var r Restrictions
	if !r.IsZero() || r.RunPlaneOnly() || !r.AgentAllowed("anything") {
		t.Errorf("zero value: IsZero=%v RunPlaneOnly=%v AgentAllowed=%v", r.IsZero(), r.RunPlaneOnly(), r.AgentAllowed("anything"))
	}
	if !MayStartAgent(Principal{}, false, "x") {
		t.Error("no principal must not be refused: a restriction belongs to a credential")
	}
}

func TestRestrictions_AgentsLimitsToTheListExactly(t *testing.T) {
	r := Restrictions{Agents: []string{"score", "review"}}
	if !r.RunPlaneOnly() {
		t.Error("an agent allowlist must confine the credential to the run plane")
	}
	for name, want := range map[string]bool{"score": true, "review": true, "Score": false, "score2": false, "": false, "judge": false} {
		if got := r.AgentAllowed(name); got != want {
			t.Errorf("AgentAllowed(%q) = %v, want %v", name, got, want)
		}
	}
	p := Principal{Restrictions: r}
	if MayStartAgent(p, true, "judge") || !MayStartAgent(p, true, "score") {
		t.Error("MayStartAgent does not follow the principal's list")
	}
}

// An empty list is refused: "no agents" and "no restriction" are opposite
// readings of it, and a mint must not pick one.
func TestRestrictions_ValidateRefusesWhatAMintMustNotGuess(t *testing.T) {
	ok := Restrictions{Agents: []string{"a", "team.member", "ns:agent-1"}}
	if err := ok.Validate(true); err != nil {
		t.Errorf("valid list refused: %v", err)
	}
	if err := (Restrictions{}).Validate(false); err != nil {
		t.Errorf("an absent list refused: %v", err)
	}
	many := make([]string, MaxRestrictedAgents+1)
	for i := range many {
		many[i] = "a" + strings.Repeat("x", i)
	}
	for name, c := range map[string]struct {
		r   Restrictions
		set bool
	}{
		"empty list":      {Restrictions{Agents: []string{}}, true},
		"nil but written": {Restrictions{}, true},
		"too many":        {Restrictions{Agents: many}, true},
		"bad name":        {Restrictions{Agents: []string{"has space"}}, true},
		"empty name":      {Restrictions{Agents: []string{""}}, true},
		"duplicate":       {Restrictions{Agents: []string{"a", "a"}}, true},
	} {
		if err := c.r.Validate(c.set); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A stored record round-trips, an unrestricted one stores as nothing, and a
// record that cannot be read is an error — never the zero value.
func TestRestrictions_StoredFormFailsClosed(t *testing.T) {
	if s, err := MarshalRestrictions(Restrictions{}); err != nil || s != "" {
		t.Errorf("zero value stored as %q (err %v), want empty", s, err)
	}
	s, err := MarshalRestrictions(Restrictions{Agents: []string{"score"}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseRestrictions(s)
	if err != nil || len(back.Agents) != 1 || back.Agents[0] != "score" {
		t.Errorf("round trip of %s = %+v (err %v)", s, back, err)
	}
	if r, err := ParseRestrictions("  "); err != nil || !r.IsZero() {
		t.Errorf("blank record = %+v (err %v), want zero", r, err)
	}
	for _, bad := range []string{`{"agents":"score"}`, `{"agents":["a"],"routes":["x"]}`, `[]`, `{`} {
		if r, err := ParseRestrictions(bad); err == nil {
			t.Errorf("record %s read as %+v with no error: an unreadable narrowing must not become none", bad, r)
		}
	}
}
