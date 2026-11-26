package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// Checkpoints, restore and fork (RFC GW §5.4, P6). A checkpoint lives on the
// workspace's worker only, beside the workspace: the core keeps no record
// of one (D-P6-5), and every op names it through the run's own workspace
// row, so the pin covers checkpoints too.

// ErrNameTaken: a fork's new name is one the run already holds.
var ErrNameTaken = errors.New("the run already holds a workspace of that name")

// maxCheckpointAnswer bounds a checkpoint op's answer: a list of at most a
// hundred records, each with at most a hundred skipped names.
const maxCheckpointAnswer = 4 << 20

// Checkpoint records the workspace's files as a new checkpoint.
func (m *Manager) Checkpoint(ctx context.Context, r store.WorkspaceRow, label string) (workspace.CheckpointAnswer, error) {
	var out workspace.CheckpointAnswer
	err := m.checkpointOp(ctx, r, workspace.OpCheckpoint, workspace.CheckpointArgs{Label: label, Max: m.limits.MaxCheckpoints}, &out)
	return out, err
}

// Checkpoints lists the workspace's checkpoints, newest first.
func (m *Manager) Checkpoints(ctx context.Context, r store.WorkspaceRow) (workspace.CheckpointsAnswer, error) {
	var out workspace.CheckpointsAnswer
	err := m.checkpointOp(ctx, r, workspace.OpCheckpoints, nil, &out)
	return out, err
}

// DeleteCheckpoint removes one of the workspace's checkpoints.
func (m *Manager) DeleteCheckpoint(ctx context.Context, r store.WorkspaceRow, checkpoint string) (workspace.DeleteCheckpointAnswer, error) {
	var out workspace.DeleteCheckpointAnswer
	err := m.checkpointOp(ctx, r, workspace.OpDeleteCheckpoint, workspace.CheckpointRef{Checkpoint: checkpoint}, &out)
	return out, err
}

// Restore puts the workspace back to a checkpoint. Its shares and checkout
// records stay: it is the same workspace (D-P6-7, D-P6-8).
func (m *Manager) Restore(ctx context.Context, r store.WorkspaceRow, checkpoint string) (workspace.RestoreAnswer, error) {
	var out workspace.RestoreAnswer
	err := m.checkpointOp(ctx, r, workspace.OpRestore, workspace.CheckpointRef{Checkpoint: checkpoint}, &out)
	return out, err
}

func (m *Manager) checkpointOp(ctx context.Context, r store.WorkspaceRow, op string, args, out any) error {
	// A root is the user's own folder: never copied into a checkpoint nor
	// replaced by one (RFC GW §4.5). The worker refuses too.
	if r.Root != "" {
		return ErrRootRefused
	}
	req := dispatch.Request{Op: op, MaxOutput: maxCheckpointAnswer}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		req.Args = b
	}
	res, err := m.Do(ctx, r, req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(res.Output, out); err != nil {
		return &dispatch.OpError{Class: workspace.ClassProtocol, Detail: "the worker's answer to " + op + " is not JSON"}
	}
	return nil
}

// Fork opens workspace as, pinned to the run, as a copy of src (the run's
// own, ready workspace) or of one of its checkpoints, on src's worker (RFC
// GW §5.4: a fork lands where the original is). It is a workspace like any
// other: its own row and session, the operator's disk quota, and counted
// against max_per_run and max_per_tenant. It has no shares and no checkout
// records of its own (D-P6-7, D-P6-8), and no checkpoints.
func (m *Manager) Fork(ctx context.Context, o Owner, src store.WorkspaceRow, as, checkpoint string) (store.WorkspaceRow, workspace.ForkAnswer, error) {
	var out workspace.ForkAnswer
	if !ValidName(as) {
		return store.WorkspaceRow{}, out, fmt.Errorf("%w: as: 1 to %d letters, digits, '.', '-' or '_', starting with a letter or digit", ErrInvalidName, MaxNameLen)
	}
	// The source must be the caller's own: the caller found it by the pin, and
	// this keeps a row of another run, tenant or owner kind (a principal's
	// workspace over MCP, RFC GW D35) from ever being forked. makeOn writes
	// the fork with the caller's kind and opening token.
	if src.OwnerRunID != o.RunID || src.TenantID != o.TenantID || src.OwnerKind != o.kind() || o.RunID == "" {
		return store.WorkspaceRow{}, out, ErrNotFound
	}
	// A fork may send requests out through the routes its source may: the
	// same owner opened the source within its grant (RFC GW §6.3).
	return m.fork(ctx, o, src, as, checkpoint, src.Egress, forkInRun)
}

// The paths a fork is made by, as its audit line names them.
const (
	forkInRun   = "fork"
	forkAtSpawn = "fork at spawn"
)

// fork makes dst's workspace as, a copy of src (a checkpoint of it, or its
// present files), with egress for its routes, and writes its audit line
// (D-P6-22) once the copy is ready. The caller has checked that src is the
// source owner's and that dst may hold the copy; path names the caller.
func (m *Manager) fork(ctx context.Context, dst Owner, src store.WorkspaceRow, as, checkpoint string, egress []string, path string) (store.WorkspaceRow, workspace.ForkAnswer, error) {
	var out workspace.ForkAnswer
	// Every fork, in a run and at spawn: a root is never copied (RFC GW
	// §4.5). The worker refuses too.
	if src.Root != "" {
		return store.WorkspaceRow{}, out, ErrRootRefused
	}
	if src.State != store.WorkspaceReady {
		return store.WorkspaceRow{}, out, fmt.Errorf("%w: %q is %s", ErrNotReady, src.Name, src.State)
	}
	if _, err := m.Get(ctx, dst, as); err == nil {
		return store.WorkspaceRow{}, out, fmt.Errorf("%w: %q", ErrNameTaken, as)
	} else if !errors.Is(err, ErrNotFound) {
		return store.WorkspaceRow{}, out, err
	}
	args, err := json.Marshal(workspace.ForkArgs{From: src.ID, Checkpoint: checkpoint, DiskQuotaBytes: m.limits.DiskQuotaBytes})
	if err != nil {
		return store.WorkspaceRow{}, out, err
	}
	// A fork is a new ephemeral workspace, whatever its source's lifetime.
	row, err := m.makeOn(ctx, dst, as, store.WorkspaceLifetimeEphemeral, "", egress, src.WorkerID, src.Engine, func(id string) error {
		res, err := m.disp.Do(ctx, src.WorkerID, dispatch.Request{TenantID: dst.TenantID, UserID: dst.UserID, WorkspaceID: id, Op: workspace.OpFork, Args: args, MaxOutput: maxCheckpointAnswer})
		if err != nil {
			return err
		}
		if json.Unmarshal(res.Output, &out) != nil {
			return &dispatch.OpError{Class: workspace.ClassProtocol, Detail: "the worker's answer to fork is not JSON"}
		}
		return nil
	})
	if err != nil {
		return store.WorkspaceRow{}, workspace.ForkAnswer{}, err
	}
	m.recordFork(src, row, path)
	return row, out, nil
}

// recordFork writes the audit line of one fork made: the source workspace
// and its run, the copy and the run that owns it. Never a file's content.
func (m *Manager) recordFork(src, fork store.WorkspaceRow, path string) {
	if m.audit == nil {
		return
	}
	_ = m.audit.Record(audit.Event{
		Action: "workspace_fork", ActorTenant: src.TenantID, ActorSubject: src.UserID,
		TargetTenant: src.TenantID, TargetName: "workspace:" + src.Name,
		WorkspaceID: src.ID, WorkspaceOwnerRunID: src.OwnerRunID, WorkspaceOwnerAgentID: src.OwnerAgentID,
		WorkspacePeerRunID: fork.OwnerRunID, WorkspacePeerAgentID: fork.OwnerAgentID, WorkspaceOp: path,
		WorkspaceForkID: fork.ID, WorkspaceForkName: fork.Name,
	}) // best effort, as for shares: a sink that fails does not undo the fork
}
