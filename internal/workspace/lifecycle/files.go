package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// The raw-byte file ops (RFC GW P2), one method each. Every one names the
// workspace by the run's Owner and the workspace's name and goes through Get
// first, so it reaches only a workspace the run itself opened (the pin), and
// only a ready one. A worker's refusal comes back as a *dispatch.OpError of
// its class (workspace.Class*: path_not_found, exists, workspace_over_quota,
// size_mismatch, copy_limit, invalid, ...); the manager's own errors are
// ErrNotFound, ErrNotReady and ErrNotSameWorker.

// ErrNotSameWorker: CopyLocal was asked to copy between two workspaces on
// different workers; the caller moves the bytes itself (GetFile, PutFile).
var ErrNotSameWorker = errors.New("the two workspaces are on different workers")

// maxAnswer bounds a JSON answer of the file ops: an fls of MaxListLimit
// entries fits well within it.
const maxAnswer = 8 << 20

// ready is the run's ready workspace of this name.
func (m *Manager) ready(ctx context.Context, o Owner, name string) (store.WorkspaceRow, error) {
	r, err := m.Get(ctx, o, name)
	if err != nil {
		return store.WorkspaceRow{}, err
	}
	if r.State != store.WorkspaceReady {
		return store.WorkspaceRow{}, fmt.Errorf("%w: %q is %s", ErrNotReady, r.Name, r.State)
	}
	return r, nil
}

// call runs one JSON op in the run's workspace name and decodes its answer
// into into (nil: none expected).
func (m *Manager) call(ctx context.Context, o Owner, name, op string, args, into any) error {
	r, err := m.ready(ctx, o, name)
	if err != nil {
		return err
	}
	return m.callRow(ctx, r, op, args, into)
}

func (m *Manager) callRow(ctx context.Context, r store.WorkspaceRow, op string, args, into any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := m.disp.Do(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: op, Args: raw, MaxOutput: maxAnswer})
	if err != nil {
		return err
	}
	return decodeAnswer(op, res.Output, into)
}

func decodeAnswer(op string, out []byte, into any) error {
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(out, into); err != nil {
		return &dispatch.OpError{Class: workspace.ClassProtocol, Detail: "a " + op + " answer that is not JSON"}
	}
	return nil
}

// Stat describes one file or folder ("" or "." is the workspace's root),
// with its sha256 when args.SHA256.
func (m *Manager) Stat(ctx context.Context, o Owner, name string, args workspace.FStatArgs) (workspace.FileInfo, error) {
	var out workspace.FileInfo
	err := m.call(ctx, o, name, workspace.OpFStat, args, &out)
	return out, err
}

// ListFiles lists a folder, one level or all of it, within args.Limit.
func (m *Manager) ListFiles(ctx context.Context, o Owner, name string, args workspace.FListArgs) (workspace.FListAnswer, error) {
	var out workspace.FListAnswer
	err := m.call(ctx, o, name, workspace.OpFList, args, &out)
	return out, err
}

// GetFile writes a file, or args.Length bytes of it from args.Offset, to
// out as it arrives, and returns the bytes written. maxBytes, when
// positive, bounds a whole-file read (output_too_large past it); a ranged
// read is bounded by its length. Nothing is held whole in memory. On an
// error out may already hold part of the file: the caller discards it.
func (m *Manager) GetFile(ctx context.Context, o Owner, name string, args workspace.GetArgs, out io.Writer, maxBytes int64) (int64, error) {
	r, err := m.ready(ctx, o, name)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return 0, err
	}
	limit := maxBytes
	if args.Length > 0 && (limit <= 0 || args.Length < limit) {
		limit = args.Length
	}
	cw := &countingWriter{w: out}
	_, err = m.disp.Stream(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: workspace.OpGet, Args: raw, MaxOutput: limit}, nil, cw)
	return cw.n, err
}

// PutFile writes exactly args.Size bytes of in to args.Path (in a stage,
// with args.Stage), streamed: the worker writes them to a temporary file
// and renames it into place only when all arrived; otherwise nothing is
// left. The quota is checked for args.Size before a byte is sent. With
// args.Size workspace.PutSizeUnknown, in is read to its end, at most
// args.MaxBytes of it, which the quota is checked for instead; nothing of
// in is read before the worker has admitted the put.
func (m *Manager) PutFile(ctx context.Context, o Owner, name string, args workspace.PutArgs, in io.Reader) (workspace.PutAnswer, error) {
	var out workspace.PutAnswer
	r, err := m.ready(ctx, o, name)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return out, err
	}
	var buf bytes.Buffer
	_, err = m.disp.Stream(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: workspace.OpPut, Args: raw,
		MaxOutput: maxAnswer, InputSize: args.Size, MaxInput: args.MaxBytes}, in, &buf)
	if err != nil {
		return out, err
	}
	return out, decodeAnswer(workspace.OpPut, buf.Bytes(), &out)
}

// Mkdir makes a folder and its missing parents (in a stage, with
// args.Stage); one already there is success.
func (m *Manager) Mkdir(ctx context.Context, o Owner, name string, args workspace.MkdirArgs) error {
	return m.call(ctx, o, name, workspace.OpMkdir, args, nil)
}

// RemoveFile removes a file, a link or a folder with what it holds; never
// the workspace's root (Release removes a workspace).
func (m *Manager) RemoveFile(ctx context.Context, o Owner, name, path string) (workspace.FRemoveAnswer, error) {
	var out workspace.FRemoveAnswer
	err := m.call(ctx, o, name, workspace.OpFRemove, workspace.FRemoveArgs{Path: path}, &out)
	return out, err
}

// Stage makes a hidden staging folder in the workspace and returns its id,
// for PutFile and Mkdir to build a folder in and Commit to place.
func (m *Manager) Stage(ctx context.Context, o Owner, name string) (string, error) {
	var out workspace.StageAnswer
	err := m.call(ctx, o, name, workspace.OpStage, struct{}{}, &out)
	return out.Stage, err
}

// Commit places a stage at args.Path in one rename (or, onto a folder there
// with args.Overwrite, merges it file by file).
func (m *Manager) Commit(ctx context.Context, o Owner, name string, args workspace.CommitArgs) (workspace.Usage, error) {
	var out workspace.Usage
	err := m.call(ctx, o, name, workspace.OpCommit, args, &out)
	return out, err
}

// Discard drops a stage; one already gone is success.
func (m *Manager) Discard(ctx context.Context, o Owner, name, stage string) error {
	return m.call(ctx, o, name, workspace.OpDiscard, workspace.DiscardArgs{Stage: stage}, nil)
}

// CopyLimits are the operator's folder-copy bounds (workspace.copy), which
// a copy that does not run on one worker (File op=copy through the core)
// applies itself.
func (m *Manager) CopyLimits() (maxFiles int, maxBytes int64) {
	return m.limits.CopyMaxFiles, m.limits.CopyMaxBytes
}

// CopyLocalRequest is a copy between two of the run's workspaces.
type CopyLocalRequest struct {
	From, FromPath string
	To, ToPath     string
	Overwrite      bool
	// Files, when not nil, is the folder's exact file list, as the caller
	// listed and judged it (workspace.CopyLocalArgs.Listed); MaxFileBytes
	// bounds each of them.
	Files        []workspace.CopyLocalFile
	MaxFileBytes int64
}

// CopyLocal copies a file or a folder from one of the run's workspaces to
// another (or within one) when both are on one worker, which does the copy
// itself under the operator's folder-copy bounds and the target's quota.
// Workspaces on different workers answer ErrNotSameWorker.
func (m *Manager) CopyLocal(ctx context.Context, o Owner, req CopyLocalRequest) (workspace.CopyAnswer, error) {
	var out workspace.CopyAnswer
	from, err := m.ready(ctx, o, req.From)
	if err != nil {
		return out, err
	}
	to, err := m.ready(ctx, o, req.To)
	if err != nil {
		return out, err
	}
	if from.WorkerID != to.WorkerID || from.TenantID != to.TenantID {
		return out, ErrNotSameWorker
	}
	err = m.callRow(ctx, to, workspace.OpCopyLocal, workspace.CopyLocalArgs{
		FromWorkspace: from.ID, FromPath: req.FromPath, Path: req.ToPath, Overwrite: req.Overwrite,
		MaxFiles: m.limits.CopyMaxFiles, MaxBytes: m.limits.CopyMaxBytes,
		Listed: req.Files != nil, Files: req.Files, MaxFileBytes: req.MaxFileBytes,
	}, &out)
	return out, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
