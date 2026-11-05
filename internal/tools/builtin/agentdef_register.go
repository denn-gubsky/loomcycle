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
// other caller is judged by the member ceiling, and may not take a name a
// definition of its tenant (or the shared tier) holds: a registered agent
// resolves before the tenant's definitions, so it would take over every run
// of that name, a schedule's included. tools is the list after the
// privileged-tool strip, which is what is stored.
func CheckRegisteredAgent(ctx context.Context, cfg *config.Config, st store.Store, req connector.RegisterAgentRequest, toolNames []string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	ctx = tools.WithCallerTier(ctx, tools.TierForPrincipal(p, ok, p.TenantID))
	if tools.IsOperatorPlane(ctx) {
		return nil
	}
	if st != nil {
		rows, err := st.AgentDefListByName(ctx, req.Name)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.TenantID == p.TenantID || r.TenantID == "" {
				return fmt.Errorf("agent name %q is not available; choose a different name", req.Name)
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
