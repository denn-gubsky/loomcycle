package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
)

// A restricted admin token cannot mint, or rotate, its way out of its
// restrictions: what it issues carries the stricter of each of its own and
// what the request asks for. An unrestricted minter is unchanged.
func TestOperatorTokenDef_AMintedTokenIsHeldToTheMintersRestrictions(t *testing.T) {
	tool, base, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	restricted := auth.WithPrincipal(base, auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeAdmin}, Restrictions: auth.Restrictions{
		Retention: retentionmode.None,
		Rate:      &auth.RateLimit{PerMinute: 6, Burst: 10},
		Overrides: &auth.OverridePolicy{Allow: []string{"effort", "model"}},
	}})
	free := auth.WithPrincipal(base, auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	restrictionsOf := func(out map[string]any) auth.Restrictions {
		t.Helper()
		row, err := st.OperatorTokenDefGet(context.Background(), out["def_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		r, err := auth.ParseRestrictions(row.RestrictionsJSON)
		if err != nil {
			t.Fatalf("stored %q: %v", row.RestrictionsJSON, err)
		}
		return r
	}
	held := func(what string, r auth.Restrictions, perMinute, burst int, allowed, refused []string) {
		t.Helper()
		if r.Retention != retentionmode.None {
			t.Errorf("%s: retention = %q, want none", what, r.Retention)
		}
		if r.Rate == nil || r.Rate.PerMinute != perMinute || r.Rate.EffectiveBurst() != burst {
			t.Errorf("%s: rate = %+v, want %d/min burst %d", what, r.Rate, perMinute, burst)
		}
		if r.Overrides == nil {
			t.Errorf("%s: overrides unlocked", what)
		}
		for _, f := range allowed {
			if !r.OverrideAllowed(f) {
				t.Errorf("%s: override %q refused, want allowed", what, f)
			}
		}
		for _, f := range refused {
			if r.OverrideAllowed(f) {
				t.Errorf("%s: override %q allowed, want refused", what, f)
			}
		}
	}

	plain := mustOp(t, tool, restricted, `{"op":"create","name":"plain","tenant_id":"acme","scopes":["runs:create"]}`)
	held("create with nothing asked", restrictionsOf(plain), 6, 10, []string{"effort", "model"}, []string{"tier"})
	looser := mustOp(t, tool, restricted, `{"op":"create","name":"looser","tenant_id":"acme","scopes":["runs:create"],"rate":{"per_minute":600},"overrides":["effort","tier"],"retention":"default"}`)
	held("create asking for more", restrictionsOf(looser), 6, 10, []string{"effort"}, []string{"model", "tier"})
	stricter := mustOp(t, tool, restricted, `{"op":"create","name":"stricter","tenant_id":"acme","scopes":["runs:create"],"rate":{"per_minute":2,"burst":3},"overrides":"none"}`)
	held("create asking for less", restrictionsOf(stricter), 2, 3, nil, []string{"effort", "model"})

	// Rotating a token less restricted than the caller would hand the caller
	// a fresh secret for it; narrowing it instead would take reach from its
	// owner. So it is refused, and the token stays as it was.
	open := mustOp(t, tool, free, `{"op":"create","name":"open","tenant_id":"acme","scopes":["runs:create"]}`)
	if r := restrictionsOf(open); !r.IsZero() {
		t.Fatalf("an unrestricted minter's token is restricted: %+v", r)
	}
	res, err := tool.Execute(restricted, json.RawMessage(`{"op":"rotate","name":"open"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "less restricted than your own") {
		t.Errorf("a restricted caller rotating an unrestricted token: %s, want it refused", res.Text)
	}
	if row, err := st.OperatorTokenDefGet(context.Background(), open["def_id"].(string)); err != nil || !row.RetiredAt.IsZero() || row.RestrictionsJSON != "" {
		t.Errorf("after the refused rotate the token is %+v (err %v), want it live and unchanged", row, err)
	}
	// A caller as restricted as the token, or less, rotates it as before.
	same := mustOp(t, tool, restricted, `{"op":"rotate","name":"plain"}`)
	held("rotate by an equally restricted caller", restrictionsOf(same), 6, 10, []string{"effort", "model"}, []string{"tier"})
	again := mustOp(t, tool, free, `{"op":"rotate","name":"plain"}`)
	held("rotate by an unrestricted caller", restrictionsOf(again), 6, 10, []string{"effort", "model"}, []string{"tier"})
	// Stricter than the caller in every field — its burst unset, which reads
	// as its per-minute rate — is not less restricted: it rotates.
	mustOp(t, tool, free, `{"op":"create","name":"paced","tenant_id":"acme","scopes":["runs:create"],"rate":{"per_minute":6},"retention":"none","overrides":["effort"]}`)
	mustOp(t, tool, restricted, `{"op":"rotate","name":"paced"}`)

	// Agents: the minter's list, intersected; nothing in common is refused.
	// (A token with agents is confined to the run plane and cannot reach the
	// mint itself; its record is checked here directly.)
	scoped := auth.WithPrincipal(base, auth.Principal{Subject: "ops2", Scopes: []string{auth.ScopeAdmin}, Restrictions: auth.Restrictions{Agents: []string{"score"}}})
	res, err = tool.Execute(scoped, json.RawMessage(`{"op":"create","name":"disjoint","tenant_id":"acme","scopes":["runs:create"],"agents":["judge"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "agents") {
		t.Errorf("a mint with no agents in common: %s, want it refused", res.Text)
	}
}
