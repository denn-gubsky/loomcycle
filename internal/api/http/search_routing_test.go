package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/search"
)

func derefB(b *bool) bool { return b != nil && *b }

func byProvider(sps []searchRoutingProvider) map[string]searchRoutingProvider {
	m := map[string]searchRoutingProvider{}
	for _, sp := range sps {
		m[sp.Provider] = sp
	}
	return m
}

// TestRouting_SearchBlock_Admin: an admin sees the flat search cascade with
// keyability (operator host key / keyless / cred) + the primary flag; the
// operator-keyed provider is selected.
func TestRouting_SearchBlock_Admin(t *testing.T) {
	srv := routingTestServer(t)
	reg, err := search.BuildRegistry([]search.ProviderSpec{
		{ID: "serper"}, {ID: "brave"}, {ID: "searxng", BaseURL: "http://sx:8080"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// serper: operator host key → keyable. brave: no host key, no cred → not
	// keyable. searxng: keyless → keyable.
	srv.SetSearchRouting(reg, search.NewResolver([]string{"serper", "brave", "searxng"}),
		map[string]string{"serper": "sk"})

	resp := routingFor(t, srv, []string{auth.ScopeAdmin})
	if len(resp.Search) != 3 {
		t.Fatalf("admin should see 3 search providers, got %d: %+v", len(resp.Search), resp.Search)
	}
	if resp.Search[0].Provider != "serper" || !resp.Search[0].Primary {
		t.Errorf("primary should be serper (cascade head): %+v", resp.Search[0])
	}
	m := byProvider(resp.Search)
	if !derefB(m["serper"].Keyable) || !derefB(m["serper"].Available) || !derefB(m["serper"].Selected) {
		t.Errorf("serper (host key, fresh) should be keyable+available+selected: %+v", m["serper"])
	}
	if derefB(m["brave"].Keyable) || derefB(m["brave"].Available) || derefB(m["brave"].Selected) {
		t.Errorf("brave (no key) should be not-keyable/available/selected: %+v", m["brave"])
	}
	if !derefB(m["searxng"].Keyable) {
		t.Errorf("searxng (keyless) should be keyable: %+v", m["searxng"])
	}
}

// TestRouting_SearchBlock_FailoverSelected: a stalled provider is keyable but not
// available, so "selected" moves to the next available provider; admin sees the
// stalled provider's last_error.
func TestRouting_SearchBlock_FailoverSelected(t *testing.T) {
	srv := routingTestServer(t)
	reg, _ := search.BuildRegistry([]search.ProviderSpec{{ID: "serper"}, {ID: "searxng", BaseURL: "http://sx"}})
	res := search.NewResolver([]string{"serper", "searxng"})
	res.MarkOutcome("serper", errors.New("boom")) // serper stalled
	srv.SetSearchRouting(reg, res, map[string]string{"serper": "sk"})

	resp := routingFor(t, srv, []string{auth.ScopeAdmin})
	m := byProvider(resp.Search)
	if derefB(m["serper"].Available) || derefB(m["serper"].Selected) {
		t.Errorf("stalled serper should be unavailable + not selected: %+v", m["serper"])
	}
	if !derefB(m["searxng"].Selected) {
		t.Errorf("searxng should be selected when serper is stalled: %+v", m["searxng"])
	}
	if m["serper"].LastError != "boom" {
		t.Errorf("admin should see serper last_error; got %q", m["serper"].LastError)
	}
}

// TestRouting_SearchBlock_RestrictedTenant: under LOOMCYCLE_OPERATOR_KEY_RESTRICTION,
// a non-admin tenant with no own credential sees only providers it can key —
// here just the keyless SearXNG (the operator's serper key is off-limits) — and
// no last_error.
func TestRouting_SearchBlock_RestrictedTenant(t *testing.T) {
	srv := routingTestServer(t)
	srv.cfg.Env.OperatorKeyRestriction = true
	reg, _ := search.BuildRegistry([]search.ProviderSpec{{ID: "serper"}, {ID: "searxng", BaseURL: "http://sx"}})
	srv.SetSearchRouting(reg, search.NewResolver([]string{"serper", "searxng"}), map[string]string{"serper": "sk"})
	srv.SetCredKeyable(func(_ context.Context, _, _, _, _ string) bool { return false }) // tenant has no own cred

	resp := routingFor(t, srv, []string{auth.ScopeTenant})
	if !resp.OperatorKeyRestricted {
		t.Error("expected operator_key_restricted=true for a restricted tenant")
	}
	if len(resp.Search) != 1 || resp.Search[0].Provider != "searxng" {
		t.Fatalf("restricted tenant should see only keyless searxng; got %+v", resp.Search)
	}
	if resp.Search[0].LastError != "" {
		t.Error("tenant must not see last_error")
	}
}

// TestRouting_SearchBlock_OmittedWhenUnconfigured: no search providers → no
// search block (back-compat: the LLM routing view is unchanged).
func TestRouting_SearchBlock_OmittedWhenUnconfigured(t *testing.T) {
	srv := routingTestServer(t) // no SetSearchRouting
	resp := routingFor(t, srv, []string{auth.ScopeAdmin})
	if resp.Search != nil {
		t.Errorf("search block should be omitted when unconfigured; got %+v", resp.Search)
	}
}
