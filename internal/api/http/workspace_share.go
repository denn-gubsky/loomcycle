package http

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// shareSpawnWorkspaces makes the workspace shares a spawn asked for (RFC GW
// §5.2, `Agent op=spawn workspaces`) with the child run just created, before
// its first turn. ctx is the spawning run's tool call: the shares are its
// own, made as it, and the Agent tool has already checked its grant and the
// arguments. The child is a run of the parent's user by construction. Any
// share that cannot be made (a name this run does not hold, a workspace not
// ready) is the spawn's error; the ones made before it stay, ending with the
// child, which then fails without a turn.
func (s *Server) shareSpawnWorkspaces(ctx context.Context, childRunID, childAgentID string) error {
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
	for _, w := range specs {
		if _, err := s.workspaces.Share(ctx, owner, w.Name, peer, w.Access); err != nil {
			return fmt.Errorf("workspaces: cannot share %q with the child: %w", w.Name, err)
		}
	}
	return nil
}
