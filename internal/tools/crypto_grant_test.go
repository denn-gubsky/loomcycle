package tools

import (
	"context"
	"reflect"
	"testing"
)

func TestEffectiveCryptoScopes_HasNoDefaultAndKeepsOnlyKnownValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared []string
		want     []string
	}{
		// Unlike memory_scopes / evaluation_scopes there is no default: every
		// key scope is reach over secret material.
		{"unset grants nothing", nil, nil},
		{"empty grants nothing", []string{}, nil},
		{"explicit deny-all", []string{DenyAllScopes}, nil},
		{"deny-all wins over a scope beside it", []string{"user", DenyAllScopes}, nil},
		{"declared order kept", []string{"tenant", "agent"}, []string{"tenant", "agent"}},
		{"every scope", []string{"agent", "user", "tenant", "run"}, []string{"agent", "user", "tenant", "run"}},
		{"duplicates removed", []string{"user", "user", "agent", "user"}, []string{"user", "agent"}},
		{"unknown values dropped", []string{"global", "user", "", "submit_any"}, []string{"user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveCryptoScopes(tc.declared); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The resolver is handed the LIVE slice off a shared config.AgentDef; a later
// append to the policy must not write into the definition.
func TestEffectiveCryptoScopes_DoesNotAliasTheDefinitionsSlice(t *testing.T) {
	declared := make([]string, 0, 4)
	declared = append(declared, "agent")
	got := EffectiveCryptoScopes(declared)
	_ = append(got, "tenant")
	if full := declared[:cap(declared)][1]; full != "" {
		t.Fatalf("an append to the policy wrote %q into the definition's backing array", full)
	}
}

func TestNarrowCryptoScopes_IsTheIntersectionInTheChildsOrder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parent, child []string
		want          []string
	}{
		{"child within parent", []string{"agent", "user", "tenant"}, []string{"tenant", "user"}, []string{"tenant", "user"}},
		// The spawn invariant: a child never gains a scope its parent lacks.
		{"child wider than parent", []string{"user"}, []string{"user", "tenant"}, []string{"user"}},
		{"parent holds none", nil, []string{"agent", "user"}, nil},
		{"child declares none", []string{"agent", "user"}, nil, nil},
		{"parent deny-all", []string{DenyAllScopes}, []string{"user"}, nil},
		{"child deny-all", []string{"user"}, []string{DenyAllScopes}, nil},
		{"disjoint", []string{"agent"}, []string{"run"}, nil},
		{"duplicates and unknowns in the child", []string{"user", "run"}, []string{"run", "bogus", "run", "user"}, []string{"run", "user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NarrowCryptoScopes(tc.parent, tc.child); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NarrowCryptoScopes(%v, %v) = %v, want %v", tc.parent, tc.child, got, tc.want)
			}
		})
	}
}

func TestCryptoPolicy_ZeroValueWhenUnsetAndAllowsOnlyGrantedScopes(t *testing.T) {
	unset := PolicyOf(context.Background()).Crypto
	if len(unset.Scopes) != 0 {
		t.Fatalf("unset policy = %v, want no scopes", unset.Scopes)
	}
	for _, s := range []string{"agent", "user", "tenant", "run"} {
		if unset.Allows(s) {
			t.Errorf("an unset policy allows %q (must default-deny)", s)
		}
	}

	ctx := WithRunPolicy(context.Background(), RunPolicy{Crypto: CryptoPolicyValue{Scopes: []string{"user", "run"}}})
	p := PolicyOf(ctx).Crypto
	for s, want := range map[string]bool{"user": true, "run": true, "agent": false, "tenant": false, "": false, DenyAllScopes: false} {
		if got := p.Allows(s); got != want {
			t.Errorf("Allows(%q) = %v, want %v", s, got, want)
		}
	}
}
