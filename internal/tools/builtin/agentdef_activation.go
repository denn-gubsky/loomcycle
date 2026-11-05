package builtin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// operatorHoldsAgentName reports whether a run of name in tenantID resolves,
// among the definitions (lookup.Agent's static and substrate tiers), to an
// operator's agent: the tenant's active version when there is one, else the
// static agent, else the shared tier's active version. It is the one answer
// register_agent and the member activation guard both ask, so they cannot
// drift (RFC FY Part B). A store fault is returned: the callers refuse.
func operatorHoldsAgentName(ctx context.Context, cfg *config.Config, st store.Store, tenantID, name string) (bool, error) {
	if st != nil && tenantID != "" {
		active, err := st.AgentDefGetActive(ctx, tenantID, name)
		switch {
		case err == nil:
			return active.OperatorAuthored, nil
		case !isNotFound(err):
			return false, err
		}
	}
	if cfg != nil {
		if _, static := cfg.Agents[name]; static {
			return true, nil
		}
	}
	if st == nil {
		return false, nil
	}
	shared, err := st.AgentDefGetActive(ctx, "", name)
	switch {
	case err == nil:
		return shared.OperatorAuthored, nil
	case isNotFound(err):
		return false, nil
	}
	return false, err
}

// operatorHoldsTeamName is operatorHoldsAgentName for a team: the tenant's
// active version, else the shared tier's (there is no static team layer).
func operatorHoldsTeamName(ctx context.Context, st store.Store, tenantID, name string) (bool, error) {
	active, err := st.TeamDefGetActive(ctx, tenantID, name)
	switch {
	case err == nil:
		return active.OperatorAuthored, nil
	case !isNotFound(err):
		return false, err
	case tenantID == "":
		return false, nil
	}
	shared, err := st.TeamDefGetActive(ctx, "", name)
	switch {
	case err == nil:
		return shared.OperatorAuthored, nil
	case isNotFound(err):
		return false, nil
	}
	return false, err
}

// memberActivationGuard refuses a member making a version of name the active
// one when a run of that name resolves to an operator's agent
// (operatorHoldsAgentName). A member may still create an inactive fork of it.
// The operator plane and a run are not narrowed here.
func (a *AgentDef) memberActivationGuard(ctx context.Context, op, tenantID, name string) error {
	if tools.IsOperatorPlane(ctx) || !tools.IsOffRunPlane(ctx) {
		return nil
	}
	held, err := operatorHoldsAgentName(ctx, a.Cfg, a.Store, tenantID, name)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if held {
		return fmt.Errorf("%s: %q is an operator's agent; you may fork it without promote, and an operator makes a version active", op, name)
	}
	return nil
}
