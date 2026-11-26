package builtin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// checkpointOp records, lists, restores or deletes a checkpoint of the
// run's own workspace (RFC GW §5.4). The worker keeps the checkpoints; the
// pin (row is the run's own) is what keeps one run from another's.
func (w *Workspace) checkpointOp(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	if (in.Op == "restore" || in.Op == "delete_checkpoint") && in.Checkpoint == "" {
		return workspaceRefusal("invalid_argument", in.Op+" needs checkpoint (checkpoints lists them)")
	}
	var (
		out any
		err error
	)
	switch in.Op {
	case "checkpoint":
		out, err = w.Manager.Checkpoint(ctx, row, in.Label)
	case "checkpoints":
		out, err = w.Manager.Checkpoints(ctx, row)
	case "restore":
		out, err = w.Manager.Restore(ctx, row, in.Checkpoint)
	case "delete_checkpoint":
		out, err = w.Manager.DeleteCheckpoint(ctx, row, in.Checkpoint)
	}
	if err != nil {
		return workspaceError(err, in.Name)
	}
	return workspaceOK(out)
}

// fork opens a new workspace of the run as a copy of its own workspace
// row, or of one of row's checkpoints, on row's worker. It needs the open
// grant, as an open does, and is refused when row's engine isolates less
// than the agent's workspace.engine requires.
func (w *Workspace) fork(ctx context.Context, o lifecycle.Owner, pol tools.WorkspacePolicyValue, row store.WorkspaceRow, in workspaceInput) tools.Result {
	if in.As == "" {
		return workspaceRefusal("invalid_argument", "fork needs as: the new workspace's name")
	}
	if pol.Engine != "" && workspace.EngineRank(row.Engine) < workspace.EngineRank(pol.Engine) {
		return workspaceRefusal("engine", fmt.Sprintf("%q runs on the %s engine; this agent requires %s", row.Name, row.Engine, pol.Engine))
	}
	if in.Branch != "" && row.Kind != store.WorkspaceKindGit {
		return workspaceRefusal("invalid_argument", "branch is for a fork of a git workspace")
	}
	fork, fa, err := w.Manager.ForkBranch(ctx, o, row, in.As, in.Checkpoint, in.Branch)
	if err != nil {
		return workspaceError(err, in.Name)
	}
	out := map[string]any{"name": fork.Name, "state": string(fork.State), "engine": fork.Engine, "forked_from": row.Name,
		"bytes": fa.Bytes, "files": fa.Files, "mechanism": fa.Mechanism, "usage": fa.Usage}
	if in.Checkpoint != "" {
		out["checkpoint"] = in.Checkpoint
	}
	withGit(out, fork.Git)
	if len(fa.Skipped) > 0 {
		out["skipped"] = fa.Skipped
	}
	if len(fa.Rewritten) > 0 {
		out["rewritten"] = fa.Rewritten
	}
	return workspaceOK(out)
}
