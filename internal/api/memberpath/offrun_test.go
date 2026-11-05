package memberpath

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func TestOperatorPlane_MembersAreNot(t *testing.T) {
	for name, c := range map[string]struct {
		p    auth.Principal
		ok   bool
		want bool
	}{
		"open mode":       {auth.Principal{}, false, true},
		"legacy":          {auth.Principal{Legacy: true}, true, true},
		"admin":           {auth.Principal{Scopes: []string{auth.ScopeAdmin}}, true, true},
		"tenant operator": {auth.Principal{Scopes: []string{auth.ScopeTenant}}, true, true},
		"member":          {auth.Principal{Scopes: []string{auth.ScopeRunsCreate}}, true, false},
		"isolated":        {auth.Principal{Scopes: []string{auth.ScopeUser}}, true, false},
	} {
		if got := OperatorPlane(c.p, c.ok); got != c.want {
			t.Errorf("%s: OperatorPlane = %v, want %v", name, got, c.want)
		}
	}
}

func TestNamesAgentScope_EveryScopeField(t *testing.T) {
	for in, want := range map[string]bool{
		`{"scope":"agent"}`:                   true,
		`{}`:                                  true, // with the agent default below
		`{"scope":"user","to_scope":"agent"}`: true,
		`{"scope":"tenant","documents_scope":"agent"}`: true,
		`{"scope":"user"}`:                     false,
		`{"scope":"tenant","to_scope":"user"}`: false,
		`"agent"`:                              false,
	} {
		if got := NamesAgentScope(json.RawMessage(in), "agent"); got != want {
			t.Errorf("NamesAgentScope(%s, agent) = %v, want %v", in, got, want)
		}
	}
	if NamesAgentScope(json.RawMessage(`{}`), "user") {
		t.Error("NamesAgentScope({}, user) = true, want false")
	}
}
