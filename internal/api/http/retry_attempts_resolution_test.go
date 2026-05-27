package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestRetryAttemptsForAgent_PerAgentOverrideWins pins the v0.12.x
// per-agent override semantics: when agent.RetryAttempts is non-nil,
// it wins over the user_tier's value, regardless of which is larger.
//
// Why this matters: high-stakes agents (cv-adapter, evaluator,
// anything with side effects) may need to refuse retries even under
// a generous tier policy. A *int field distinguishes "unset = use
// tier" from "0 = explicitly disable retries" — without the pointer
// the yaml-omitted case (operator wants tier default) and the
// yaml-zero case (operator wants no retries) would collapse to the
// same value.
func TestRetryAttemptsForAgent_PerAgentOverrideWins(t *testing.T) {
	zero := 0
	five := 5

	cases := []struct {
		name       string
		tier       string
		tierBudget int
		agent      *int // nil = field unset
		want       int
	}{
		{
			name:       "agent unset → tier value",
			tier:       "paid",
			tierBudget: 2,
			agent:      nil,
			want:       2,
		},
		{
			name:       "agent override forces 0 even under generous tier",
			tier:       "paid",
			tierBudget: 5,
			agent:      &zero,
			want:       0,
		},
		{
			name:       "agent override raises above tier",
			tier:       "free",
			tierBudget: 0,
			agent:      &five,
			want:       5,
		},
		{
			name:       "agent unset + tier unset → 0",
			tier:       "free",
			tierBudget: 0,
			agent:      nil,
			want:       0,
		},
		{
			name:       "unknown tier + agent override → agent wins",
			tier:       "nonexistent",
			tierBudget: 0,
			agent:      &five,
			want:       5,
		},
		{
			name:       "unknown tier + agent unset → 0",
			tier:       "nonexistent",
			tierBudget: 0,
			agent:      nil,
			want:       0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{
				cfg: &config.Config{
					UserTiers: map[string]config.UserTier{
						"paid": {RetryAttempts: tc.tierBudget},
						"free": {RetryAttempts: tc.tierBudget},
					},
				},
			}
			agentDef := config.AgentDef{RetryAttempts: tc.agent}
			got := srv.retryAttemptsForAgent(agentDef, tc.tier)
			if got != tc.want {
				t.Errorf("retryAttemptsForAgent(agent=%v, tier=%q with budget=%d) = %d, want %d",
					tc.agent, tc.tier, tc.tierBudget, got, tc.want)
			}
		})
	}
}

// TestRetryAttemptsForAgent_NilCfgFallsThroughToZero pins the safety
// path: if the Server has no config wired (test fixtures, partial
// init), the helper returns 0 rather than panicking on nil-map access.
func TestRetryAttemptsForAgent_NilCfgFallsThroughToZero(t *testing.T) {
	srv := &Server{cfg: nil}
	agentDef := config.AgentDef{} // no override

	got := srv.retryAttemptsForAgent(agentDef, "anything")
	if got != 0 {
		t.Errorf("nil cfg + no override should be 0, got %d", got)
	}

	// With an override the cfg is irrelevant.
	zero := 0
	five := 5
	if got := srv.retryAttemptsForAgent(config.AgentDef{RetryAttempts: &zero}, "anything"); got != 0 {
		t.Errorf("nil cfg + agent override 0 should be 0, got %d", got)
	}
	if got := srv.retryAttemptsForAgent(config.AgentDef{RetryAttempts: &five}, "anything"); got != 5 {
		t.Errorf("nil cfg + agent override 5 should be 5, got %d", got)
	}
}
