package http

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// spawnWorkspaces makes the workspace shares and forks a spawn asked for
// (`Agent op=spawn workspaces`) for the child run just created, before its
// first turn. ctx is the spawning run's tool call: the shares (RFC GW §5.2)
// are its own, made as it, and each fork (§5.4) copies one of its own
// workspaces; the Agent tool has already checked its grant and the
// arguments. childCtx is the child's run context, which carries the child's
// identity and its narrowed policy. The child is a run of the parent's user
// by construction. Any share or fork that cannot be made (a name this run
// does not hold, a workspace not ready, a limit) is the spawn's error; the
// ones made before it stay, ending with the child, which then fails
// without a turn.
func (s *Server) spawnWorkspaces(ctx, childCtx context.Context, childRunID, childAgentID string) error {
	specs := tools.SpawnWorkspaces(ctx)
	if len(specs) == 0 {
		return nil
	}
	if s.workspaces == nil {
		return fmt.Errorf("workspaces: this runtime serves no workspaces (workspace.mode is off)")
	}
	ident := tools.RunIdentity(ctx)
	owner := lifecycle.Owner{TenantID: ident.TenantID, UserID: ident.UserID, RunID: tools.RunID(ctx), AgentID: ident.AgentID, RootRunID: ident.RootRunID}
	peer := lifecycle.Peer{RunID: childRunID, AgentID: childAgentID}
	cident := tools.RunIdentity(childCtx)
	child := lifecycle.Owner{TenantID: cident.TenantID, UserID: cident.UserID, RunID: childRunID, AgentID: childAgentID, RootRunID: cident.RootRunID}
	cpol := tools.PolicyOf(childCtx).Workspace
	for _, w := range specs {
		if w.Fork {
			if _, err := s.workspaces.ForkForChild(ctx, owner, lifecycle.ChildFork{Child: child, Name: w.Name, As: w.As, Egress: cpol.Egress, Engine: cpol.Engine}); err != nil {
				return fmt.Errorf("workspaces: cannot fork %q for the child: %w", w.Name, err)
			}
			continue
		}
		if _, err := s.workspaces.Share(ctx, owner, w.Name, peer, w.Access); err != nil {
			return fmt.Errorf("workspaces: cannot share %q with the child: %w", w.Name, err)
		}
	}
	return nil
}
