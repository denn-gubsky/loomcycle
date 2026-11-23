package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	// The source must be the run's own: the caller found it by the pin, and
	// this keeps a row of another run or tenant from ever being forked.
	if src.OwnerRunID != o.RunID || src.TenantID != o.TenantID || o.RunID == "" {
		return store.WorkspaceRow{}, out, ErrNotFound
	}
	if src.State != store.WorkspaceReady {
		return store.WorkspaceRow{}, out, fmt.Errorf("%w: %q is %s", ErrNotReady, src.Name, src.State)
	}
	if _, err := m.Get(ctx, o, as); err == nil {
		return store.WorkspaceRow{}, out, fmt.Errorf("%w: %q", ErrNameTaken, as)
	} else if !errors.Is(err, ErrNotFound) {
		return store.WorkspaceRow{}, out, err
	}
	args, err := json.Marshal(workspace.ForkArgs{From: src.ID, Checkpoint: checkpoint, DiskQuotaBytes: m.limits.DiskQuotaBytes})
	if err != nil {
		return store.WorkspaceRow{}, out, err
	}
	row, err := m.makeOn(ctx, o, as, src.WorkerID, src.Engine, func(id string) error {
		res, err := m.disp.Do(ctx, src.WorkerID, dispatch.Request{TenantID: o.TenantID, WorkspaceID: id, Op: workspace.OpFork, Args: args, MaxOutput: maxCheckpointAnswer})
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
	return row, out, nil
}
