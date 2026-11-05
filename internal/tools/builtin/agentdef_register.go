package builtin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// CheckRegisteredAgent holds a register_agent definition to what an AgentDef
// create is held to, for the caller on ctx: a run resolves a registered agent
// exactly as a defined one (lookup.Agent), with its own crypto, statement
// method, evaluation and git grants. The operator plane is not narrowed; any
// other caller is judged by the member ceiling, and may not take a name an
// operator's agent holds (operatorHoldsAgentName: the tenant's or the shared
// tier's operator version, or a static agent) or any definition of its tenant
// or the shared tier holds: a tenant's registered agent resolves before its
// definitions and the static agents, so it would take over every run of that
// name, a schedule's included. tools is the list after the
// privileged-tool strip, which is what is stored.
func CheckRegisteredAgent(ctx context.Context, cfg *config.Config, st store.Store, req connector.RegisterAgentRequest, toolNames []string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	ctx = tools.WithCallerTier(ctx, tools.TierForPrincipal(p, ok, p.TenantID))
	if tools.IsOperatorPlane(ctx) {
		return nil
	}
	unavailable := fmt.Errorf("agent name %q is not available; choose a different name", req.Name)
	if held, err := operatorHoldsAgentName(ctx, cfg, st, p.TenantID, req.Name); err != nil {
		return err
	} else if held {
		return unavailable
	}
	// Beyond the operator's: any definition of the name in the tenant or the
	// shared tier, a member's included — a registration would shadow it too.
	if st != nil {
		rows, err := st.AgentDefListByName(ctx, req.Name)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.TenantID == p.TenantID || r.TenantID == "" {
				return unavailable
			}
		}
	}
	a := &AgentDef{Store: st, Cfg: cfg}
	return a.checkCapabilityCeiling(ctx, req.Name, mergedDef{
		Provider:         req.Provider,
		Tools:            toolNames,
		MemoryScopes:     req.MemoryScopes,
		Channels:         req.Channels,
		EvaluationScopes: req.EvaluationScopes,
		Interruption:     req.Interruption,
		CryptoScopes:     req.CryptoScopes,
		ShellCredentials: req.ShellCredentials,
		GitRemote:        req.GitRemote,
		StatementMethods: req.StatementMethods,
	}, nil)
}
