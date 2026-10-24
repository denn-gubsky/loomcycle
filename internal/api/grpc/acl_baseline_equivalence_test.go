package grpc

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY §11 / RFC EZ §2 — the baseline equivalence test, gRPC half: for
// every RPC the service descriptor registers and every principal class, the
// class's built-in baseline reproduces what enforceScope (the scope gate,
// the credential self-service carve-out and the run-plane allowlist) admits.
// The HTTP half (internal/api/http) says why the two directions are
// asserted the way they are, and what a collision is. One more difference is
// logged rather than failed here: gRPC has no member path (RFC EZ §7 Q1 is
// open), so a member's substrate:tenant RPC is refused by the scope ceiling
// while its HTTP twin admits the member, and the baseline carries the HTTP
// reach.

// noACLRows is an ACL store with nothing authored: every principal's set is
// its class baseline alone, compiled by the real resolver.
type noACLRows struct{}

func (noACLRows) GroupsOfSubject(context.Context, string, string) ([]string, error) { return nil, nil }
func (noACLRows) GroupGraph(context.Context, string) (map[string][]string, error)   { return nil, nil }
func (noACLRows) AclBindingsFor(context.Context, string, string, []string, string) ([]store.AclBindingRow, error) {
	return nil, nil
}
func (noACLRows) AclDefGet(context.Context, string, string) (store.AclDefRow, error) {
	return store.AclDefRow{}, &store.ErrNotFound{}
}
func (noACLRows) AclVersion(context.Context, string) (int64, error) { return 1, nil }

type eqClass struct {
	name    string
	scopes  []string
	agents  []string
	maximal bool
}

var eqMemberScopes = []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeRunsDelete,
	auth.ScopeChannelPublish, auth.ScopeChannelRead, auth.ScopeLLMGateway, auth.ScopeProvidersOperatorKey}

// The classes of the HTTP half, less the legacy secret (on gRPC it resolves
// to the same substrate:admin principal as the admin token).
func eqClasses() []eqClass {
	return []eqClass{
		{name: "admin", scopes: []string{auth.ScopeAdmin}, maximal: true},
		{name: "tenant", scopes: []string{auth.ScopeTenant}, maximal: true},
		{name: "tenant+runplane", scopes: []string{auth.ScopeTenant}, agents: []string{"chat"}},
		{name: "member-full", scopes: eqMemberScopes, maximal: true},
		{name: "member-runs", scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{name: "member-read", scopes: []string{auth.ScopeRunsRead}},
		{name: "member-channels", scopes: []string{auth.ScopeChannelPublish, auth.ScopeChannelRead}},
		{name: "member-none", scopes: []string{}},
		{name: "member+runplane", scopes: eqMemberScopes, agents: []string{"chat"}},
		{name: "isolated-full", scopes: append([]string{auth.ScopeUser}, eqMemberScopes...), maximal: true},
		{name: "isolated", scopes: []string{auth.ScopeUser}},
		{name: "isolated+runplane", scopes: append([]string{auth.ScopeUser}, eqMemberScopes...), agents: []string{"chat"}},
	}
}

// baselineWiderThanGateGRPC lists the (class, question) pairs where the
// class's baseline allows an RPC's question though enforceScope refuses even
// the widest token of the class, with no collision or member-path reason.
// Keyed "<class>|<action> <type>:<name>". Each needs a reason; keep it
// near-empty.
var baselineWiderThanGateGRPC = map[string]string{
	// An isolated user's own runs reach its keyspaces and documents (the
	// in-run intersection asks these questions of the same baseline); the
	// off-run RPCs ask the same type-level question and need substrate:tenant.
	"isolated-full|read document:*": "the class's in-run reach asks the same question as an off-run RPC the scope gate refuses it",
	"isolated-full|read memory:*":   "the class's in-run reach asks the same question as an off-run RPC the scope gate refuses it",
	// POST /v1/_history admits an isolated user (runs:read, the handler caps
	// it to its own scopes); the History RPC requires substrate:tenant. A
	// gate difference between the transports, reported; the baseline follows
	// HTTP.
	"isolated-full|read history:*": "HTTP admits it, the History RPC requires substrate:tenant (a gate difference, reported)",
}

func questionKey(s authz.Surface) string { return s.Action + " " + s.Type + ":" + s.Name }

// isMemberClass: a non-isolated member token (no substrate:tenant).
func isMemberClass(c eqClass) bool {
	p := auth.Principal{Scopes: c.scopes}
	return !auth.HasScope(c.scopes, auth.ScopeTenant) && !auth.IsIsolated(p, true)
}

func (c eqClass) principal(t *testing.T, mode authz.Mode) auth.Principal {
	t.Helper()
	p := auth.Principal{TenantID: "acme", Subject: c.name, TokenDefID: "def_" + c.name, Scopes: c.scopes,
		Restrictions: auth.Restrictions{Agents: c.agents}}
	out, err := aclres.New(noACLRows{}, mode, 0).Resolve(context.Background(), p)
	if err != nil || out.Effective == nil {
		t.Fatalf("class %s: resolve: %v", c.name, err)
	}
	return out
}

func TestACLBaseline_GRPCRPCsMatchTheLegacyGatePerClass(t *testing.T) {
	rpcs := serviceRPCs()
	// No set on the principal: the gate decides as before RFC EY.
	legacyOf := func(c eqClass, rpc string) bool {
		bare := c.principal(t, authz.ModeCompat)
		bare.Effective = nil
		return enforceScope(auth.WithPrincipal(context.Background(), bare), grpcMethodPrefix+rpc) == nil
	}
	askedOK := map[string]map[string]string{}
	for _, rpc := range rpcs {
		surface, governed := authz.GRPCSurface(rpc)
		for _, c := range eqClasses() {
			if governed && legacyOf(c, rpc) {
				if askedOK[c.name] == nil {
					askedOK[c.name] = map[string]string{}
				}
				askedOK[c.name][questionKey(surface)] = rpc
			}
		}
	}

	var narrowing, widening, logged []string
	usedAllow := map[string]bool{}
	admitted := map[string]int{}
	for _, rpc := range rpcs {
		full := grpcMethodPrefix + rpc
		surface, governed := authz.GRPCSurface(rpc)
		for _, c := range eqClasses() {
			legacy := legacyOf(c, rpc)
			if legacy {
				admitted[c.name]++
			}
			for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
				p := c.principal(t, mode)
				ctx := auth.WithPrincipal(authz.WithTransport(authz.WithMode(context.Background(), mode), "grpc"), p)
				err := enforceScope(ctx, full)
				if got := err == nil; got != legacy {
					q := "(ungoverned)"
					if governed {
						d := authz.EvaluateSurface(authz.WithMode(context.Background(), authz.ModeEnforce), p.Effective, surface, false).Decision
						q = fmt.Sprintf("asks %s %s:%s → %s %v", surface.Action, surface.Type, surface.Name, d.Outcome, d.Reasons)
					}
					narrowing = append(narrowing, fmt.Sprintf("%s: %s — the legacy gate admits=%v, %s mode admits=%v (%v); %s",
						c.name, rpc, legacy, mode, got, err, q))
				}
			}
			if !legacy && governed && c.maximal {
				p := c.principal(t, authz.ModeEnforce)
				res := authz.EvaluateSurface(authz.WithMode(context.Background(), authz.ModeEnforce), p.Effective, surface, false)
				if !res.Allowed {
					continue
				}
				q := questionKey(surface)
				key := c.name + "|" + q
				switch {
				case askedOK[c.name][q] != "":
					logged = append(logged, fmt.Sprintf("collision: %s: %s asks %q, as %s does", c.name, rpc, q, askedOK[c.name][q]))
				case isMemberClass(c) && requiredScopeForRPC(full) == auth.ScopeTenant:
					logged = append(logged, fmt.Sprintf("no member path on gRPC: %s: %s (%q)", c.name, rpc, q))
				case baselineWiderThanGateGRPC[key] != "":
					usedAllow[key] = true
				default:
					widening = append(widening, fmt.Sprintf("%s: %s — enforceScope refuses the widest token of the class, the baseline allows %s (%v)",
						c.name, rpc, q, res.Decision.Reasons))
				}
			}
		}
	}
	for key := range baselineWiderThanGateGRPC {
		if !usedAllow[key] {
			widening = append(widening, fmt.Sprintf("%s is allowlisted as baseline-wider but no RPC needs it any more; remove the entry", key))
		}
	}
	sort.Strings(narrowing)
	sort.Strings(widening)
	sort.Strings(logged)
	for _, d := range narrowing {
		t.Errorf("narrowing: %s", d)
	}
	for _, d := range widening {
		t.Errorf("widening: %s", d)
	}
	for _, d := range logged {
		t.Logf("%s", d)
	}
	for _, c := range eqClasses() {
		if admitted[c.name] == 0 {
			t.Errorf("class %s was admitted to no RPC; the fixture is wrong", c.name)
		}
	}
	if admitted["admin"] != len(rpcs) {
		t.Errorf("admin admitted to %d of %d RPCs", admitted["admin"], len(rpcs))
	}
}
