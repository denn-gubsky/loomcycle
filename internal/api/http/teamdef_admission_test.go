package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// admitTeamRun is the op=run admission gate (RFC AP review finding #1): op=run
// does not pass through RunOnce, so this enforces the agent-depth bound, the RFC
// AW token budget, and the RFC AX operator-key restriction before a team walk.

func TestAdmitTeamRun_RefusesAtMaxDepth(t *testing.T) {
	s := &Server{cfgHolder: config.NewHolder(&config.Config{}), limits: limits.New(nil)}
	ctx := context.Background()
	for i := 0; i < builtin.MaxAgentDepth; i++ {
		ctx = builtin.IncrementAgentDepth(ctx)
	}
	if _, err := s.admitTeamRun(ctx); err == nil || !strings.Contains(err.Error(), "max agent depth") {
		t.Fatalf("op=run at max depth should be refused, got %v", err)
	}
}

func TestAdmitTeamRun_AllowsAndIncrementsDepth(t *testing.T) {
	s := &Server{cfgHolder: config.NewHolder(&config.Config{}), limits: limits.New(nil)} // no-op tracker → always allowed
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "u1"})

	out, err := s.admitTeamRun(ctx)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	// The walk counts as one nesting level so its spawned agents are bounded.
	if got := builtin.AgentDepth(out); got != 1 {
		t.Errorf("admitted depth = %d, want 1", got)
	}
	// No principal + gate off → not restricted → operator key allowed on the ctx.
	if !providers.OperatorKeyAllowed(out) {
		t.Errorf("unrestricted run should allow the operator key")
	}
}

// A team walked from inside a confined run stays confined even when the ctx
// carries a looser principal — a draft started by another user keeps its
// creator's bits on the run while ctx holds the starter's token. The principal
// winning would let the walk's agents spend the operator's key and reach the
// tenant's shared data.
func TestAdmitTeamRun_KeepsTheRunsConfinementUnderALooserPrincipal(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.OperatorKeyRestriction = true
	s := &Server{cfgHolder: config.NewHolder(cfg), limits: limits.New(nil)}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		TenantID: "acme", UserID: "alice", OperatorKeyRestricted: true, Isolated: true,
	})
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}})

	out, err := s.admitTeamRun(ctx)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if id := tools.RunIdentity(out); !id.OperatorKeyRestricted || !id.Isolated {
		t.Errorf("walk admitted with restricted=%v isolated=%v, want both", id.OperatorKeyRestricted, id.Isolated)
	}
	if providers.OperatorKeyAllowed(out) {
		t.Error("walk admitted with the operator key allowed")
	}
}

// A walk from a resumed confined run (no principal on ctx) stays confined, and
// a restricted principal still restricts a walk from a plain ctx.
func TestAdmitTeamRun_ConfinedByEitherTheRunOrThePrincipal(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.OperatorKeyRestriction = true
	s := &Server{cfgHolder: config.NewHolder(cfg), limits: limits.New(nil)}
	resumed := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		TenantID: "acme", UserID: "alice", OperatorKeyRestricted: true, Isolated: true,
	})
	principal := auth.WithPrincipal(tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "alice"}),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}})
	for name, ctx := range map[string]context.Context{"resumed run": resumed, "restricted principal": principal} {
		out, err := s.admitTeamRun(ctx)
		if err != nil {
			t.Fatalf("%s: admit: %v", name, err)
		}
		if id := tools.RunIdentity(out); !id.OperatorKeyRestricted || !id.Isolated || providers.OperatorKeyAllowed(out) {
			t.Errorf("%s: walk admitted with restricted=%v isolated=%v opKeyAllowed=%v, want confined",
				name, id.OperatorKeyRestricted, id.Isolated, providers.OperatorKeyAllowed(out))
		}
	}
}
