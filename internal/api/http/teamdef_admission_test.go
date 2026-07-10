package http

import (
	"context"
	"strings"
	"testing"

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
	s := &Server{cfg: &config.Config{}, limits: limits.New(nil)}
	ctx := context.Background()
	for i := 0; i < builtin.MaxAgentDepth; i++ {
		ctx = builtin.IncrementAgentDepth(ctx)
	}
	if _, err := s.admitTeamRun(ctx); err == nil || !strings.Contains(err.Error(), "max agent depth") {
		t.Fatalf("op=run at max depth should be refused, got %v", err)
	}
}

func TestAdmitTeamRun_AllowsAndIncrementsDepth(t *testing.T) {
	s := &Server{cfg: &config.Config{}, limits: limits.New(nil)} // no-op tracker → always allowed
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
