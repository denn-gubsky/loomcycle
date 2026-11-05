package builtin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memberActivationGuard refuses a member making a version of name the active
// one when the agent a run of that name resolves to now is an operator's: its
// active version is operator-authored, or (none in the caller's tenant) a
// shared operator-authored version or a static agent holds the name. A member
// may still create an inactive fork of it. The operator plane and a run are not
// narrowed here.
func (a *AgentDef) memberActivationGuard(ctx context.Context, op, tenantID, name string) error {
	if tools.IsOperatorPlane(ctx) || !tools.IsOffRunPlane(ctx) {
		return nil
	}
	refuse := fmt.Errorf("%s: %q is an operator's agent; you may fork it without promote, and an operator makes a version active", op, name)
	active, err := a.Store.AgentDefGetActive(ctx, tenantID, name)
	switch {
	case err == nil:
		if active.OperatorAuthored {
			return refuse
		}
		return nil
	case !isNotFound(err):
		return fmt.Errorf("%s: %w", op, err)
	}
	if tenantID != "" {
		if shared, serr := a.Store.AgentDefGetActive(ctx, "", name); serr == nil && shared.OperatorAuthored {
			return refuse
		} else if serr != nil && !isNotFound(serr) {
			return fmt.Errorf("%s: %w", op, serr)
		}
	}
	if a.Cfg != nil {
		if _, static := a.Cfg.Agents[name]; static {
			return refuse
		}
	}
	return nil
}
