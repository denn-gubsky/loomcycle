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

// The override policy: nil locks nothing, "none" locks every override, a list
// allows exactly what it names.
func TestRestrictions_OverridePolicy(t *testing.T) {
	if !(Restrictions{}).OverrideAllowed("model") {
		t.Error("no policy must allow every override")
	}
	none := Restrictions{Overrides: &OverridePolicy{}}
	if none.IsZero() {
		t.Error(`a "none" policy is a restriction; IsZero said otherwise`)
	}
	for _, f := range LockableOverrides {
		if none.OverrideAllowed(f) {
			t.Errorf(`"none" allows %q`, f)
		}
	}
	some := Restrictions{Overrides: &OverridePolicy{Allow: []string{"effort"}}}
	if !some.OverrideAllowed("effort") || some.OverrideAllowed("model") {
		t.Error("a list policy must allow exactly what it names")
	}
	if none.RunPlaneOnly() {
		t.Error("an override lock alone must not confine a token to the run plane")
	}
}

func TestRestrictions_OverridePolicyOnTheWire(t *testing.T) {
	for in, want := range map[string]string{
		`{"overrides":"none"}`:                `{"overrides":"none"}`,
		`{"overrides":["effort","tier"]}`:     `{"overrides":["effort","tier"]}`,
		`{"agents":["a"],"overrides":"none"}`: `{"agents":["a"],"overrides":"none"}`,
	} {
		r, err := ParseRestrictions(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if r.Overrides == nil {
			t.Errorf("%s: policy lost on read", in)
		}
		if out, err := MarshalRestrictions(r); err != nil || out != want {
			t.Errorf("%s: stored as %s (err %v), want %s", in, out, err, want)
		}
	}
	// A policy that cannot be read is an error, never a looser policy.
	for _, bad := range []string{`{"overrides":"all"}`, `{"overrides":[]}`, `{"overrides":true}`, `{"overrides":{"allow":["x"]}}`} {
		if r, err := ParseRestrictions(bad); err == nil {
			t.Errorf("%s read as %+v with no error", bad, r)
		}
	}
	for name, r := range map[string]Restrictions{
		"unknown field": {Overrides: &OverridePolicy{Allow: []string{"temperature"}}},
		"not lockable":  {Overrides: &OverridePolicy{Allow: []string{"tools"}}},
		"duplicate":     {Overrides: &OverridePolicy{Allow: []string{"effort", "effort"}}},
	} {
		if err := r.Validate(false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (Restrictions{Overrides: &OverridePolicy{Allow: []string{"effort", "sampling"}}}).Validate(false); err != nil {
		t.Errorf("a valid list refused: %v", err)
	}
	for in, wantAllow := range map[string]int{"none": 0, "effort, tier": 2} {
		p, err := ParseOverridePolicy(in)
		if err != nil || len(p.Allow) != wantAllow {
			t.Errorf("ParseOverridePolicy(%q) = %+v (err %v)", in, p, err)
		}
	}
	if _, err := ParseOverridePolicy(" , "); err == nil {
		t.Error("an empty text policy was accepted")
	}
}

func TestRestrictions_Rate(t *testing.T) {
	r := Restrictions{Rate: &RateLimit{PerMinute: 6, Burst: 20}}
	if r.IsZero() || r.RunPlaneOnly() {
		t.Errorf("a rate alone: IsZero=%v RunPlaneOnly=%v, want a restriction that does not confine the token", r.IsZero(), r.RunPlaneOnly())
	}
	if err := r.Validate(false); err != nil {
		t.Errorf("a valid rate refused: %v", err)
	}
	if got := (RateLimit{PerMinute: 6}).EffectiveBurst(); got != 6 {
		t.Errorf("burst defaults to %d, want per_minute", got)
	}
	for name, bad := range map[string]RateLimit{
		"zero rate": {}, "negative": {PerMinute: -1}, "negative burst": {PerMinute: 1, Burst: -1},
		"absurd rate": {PerMinute: MaxRatePerMinute + 1}, "absurd burst": {PerMinute: 1, Burst: MaxRateBurst + 1},
	} {
		rate := bad
		if err := (Restrictions{Rate: &rate}).Validate(false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s, err := MarshalRestrictions(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseRestrictions(s)
	if err != nil || back.Rate == nil || back.Rate.PerMinute != 6 || back.Rate.Burst != 20 {
		t.Errorf("round trip of %s = %+v (err %v)", s, back.Rate, err)
	}
	if _, err := ParseRestrictions(`{"rate":{"per_minute":6,"per_hour":1}}`); err == nil {
		t.Error("a rate with an unknown key was read without error")
	}
}
