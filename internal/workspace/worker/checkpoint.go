package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// checkpointsDir is the folder of the worker's root that holds checkpoints
// (RFC GW §5.4): <root>/.loomcycle-checkpoints/<workspace id>/<checkpoint
// id>/ is a checkpoint's files, and <checkpoint id>.json beside it its
// record, written last. Beside the workspaces, never in one: a session
// mounts only its workspace's folder and a file op is confined to it, so
// nothing an agent runs can read or change a checkpoint, or the record that
// counts it against the quota. A dot in its name keeps held() from taking
// it for a workspace.
const checkpointsDir = ".loomcycle-checkpoints"

// checkpointTmp, inside checkpointsDir, is where a checkpoint, a restore's
// new tree and a fork are built before a rename puts each in place, so a
// worker killed part way leaves nothing half made anywhere else; start-up
// clears it.
const checkpointTmp = ".tmp"

const checkpointPrefix = "ck_"

// probePrefix names the folder the mechanism is probed in, in the root.
const probePrefix = ".loomcycle-probe-"

func (w *worker) checkpointsRoot() string { return filepath.Join(w.opt.Root, checkpointsDir) }

// checkpointsOf is workspace id's folder of checkpoints; id is a ValidID.
func (w *worker) checkpointsOf(id string) string { return filepath.Join(w.checkpointsRoot(), id) }

// tmpDir makes and returns a new folder name in checkpointTmp; the folder
// itself is not made.
func (w *worker) tmpPath(kind string) (string, error) {
	dir := filepath.Join(w.checkpointsRoot(), checkpointTmp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, kind+"-"+hex.EncodeToString(b[:])), nil
}

// validCheckpointID reports whether ck may name a checkpoint: what the
// worker mints, so it is never a path.
func validCheckpointID(ck string) bool {
	return strings.HasPrefix(ck, checkpointPrefix) && workspace.ValidID(ck)
}

func newCheckpointID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return checkpointPrefix + hex.EncodeToString(b[:]), nil
}

// checkpointList reads workspace id's checkpoint records, newest first. A
// record without its folder, or one that does not parse, is not a
// checkpoint.
func (w *worker) checkpointList(id string) []workspace.CheckpointInfo {
	dir := w.checkpointsOf(id)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []workspace.CheckpointInfo
	for _, e := range ents {
		ck, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !validCheckpointID(ck) {
			continue
		}
		if info, ok := w.readCheckpoint(id, ck); ok {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, func(a, b workspace.CheckpointInfo) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// readCheckpoint reads one record; ok is false when ck is not a checkpoint
// of workspace id.
func (w *worker) readCheckpoint(id, ck string) (workspace.CheckpointInfo, bool) {
	if !validCheckpointID(ck) {
		return workspace.CheckpointInfo{}, false
	}
	// #nosec G304 -- id and ck are each checked by ValidID (no separator, no dot), so the path stays in the checkpoints folder
	b, err := os.ReadFile(filepath.Join(w.checkpointsOf(id), ck+".json"))
	if err != nil {
		return workspace.CheckpointInfo{}, false
	}
	var info workspace.CheckpointInfo
	if json.Unmarshal(b, &info) != nil || info.ID != ck || info.Bytes < 0 {
		return workspace.CheckpointInfo{}, false
	}
	if st, err := os.Lstat(filepath.Join(w.checkpointsOf(id), ck)); err != nil || !st.IsDir() {
		return workspace.CheckpointInfo{}, false
	}
	return info, true
}

// checkpointBytes is what workspace id's checkpoints count against its
// quota.
func (w *worker) checkpointBytes(id string) int64 {
	var n int64
	for _, c := range w.checkpointList(id) {
		n += c.Bytes
	}
	return n
}

// dropCheckpoints removes workspace id's checkpoints: the workspace is
// gone, and a checkpoint goes with it (RFC GW §5.4).
func (w *worker) dropCheckpoints(id string) error {
	return removeDir(w.checkpointsOf(id))
}

// validLabel is a label of at most MaxCheckpointLabel printable characters.
func validLabel(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > workspace.MaxCheckpointLabel {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// checkpointOp records the workspace's files as a new checkpoint. Under the
// write lock the workspace is measured afresh, and the copy's budget is the
// room its quota leaves beside the files and the checkpoints already held:
// a checkpoint counts its full size, so it can never be a way round the
// quota (RFC GW P6, D-P6-4).
func (w *worker) checkpointOp(ctx context.Context, id, real string, raw []byte) answer {
	var in workspace.CheckpointArgs
	if len(raw) > 0 {
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
	}
	if !validLabel(in.Label) {
		return refuse(workspace.ClassInvalid, "label: at most %d printable characters", workspace.MaxCheckpointLabel)
	}
	limit := in.Max
	if limit <= 0 {
		limit = workspace.DefaultMaxCheckpoints
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	if n := len(w.checkpointList(id)); n >= limit {
		return refuse(workspace.ClassCheckpointLimit, "the workspace holds %d checkpoints, as many as it may; delete one first (checkpoints lists them)", n)
	}
	w.measureLocked(ctx, u, v)
	if bad := u.admit(u.used); bad != nil {
		bad.detail = "a checkpoint counts its full size against the quota: " + bad.detail
		return *bad
	}
	ck, err := newCheckpointID()
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	tmp, err := w.tmpPath("checkpoint")
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	res, err := w.copyTree(ctx, real, tmp, u.limit-u.used-u.held)
	if errors.Is(err, errOverBudget) {
		return refuse(workspace.ClassOverQuota, "the workspace grew while it was recorded and its checkpoint would pass the quota of %d bytes; nothing was kept", u.limit)
	}
	if err != nil {
		return refuse(workspace.ClassFailed, "recording the checkpoint: %v", err)
	}
	info := workspace.CheckpointInfo{ID: ck, Label: in.Label, CreatedAt: time.Now().UTC(), Bytes: res.bytes, Files: res.files, Mechanism: w.mechanism(), Skipped: res.skipped}
	if err := w.placeCheckpoint(id, tmp, info); err != nil {
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "recording the checkpoint: %v", err)
	}
	u.held += res.bytes
	u.full.Store(u.over())
	return jsonAnswer(workspace.CheckpointAnswer{CheckpointInfo: info, Usage: u.snapshot()})
}

// placeCheckpoint renames a built checkpoint into place and writes its
// record last: a folder with no record is one that did not finish.
func (w *worker) placeCheckpoint(id, tmp string, info workspace.CheckpointInfo) error {
	dir := w.checkpointsOf(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, info.ID)); err != nil {
		return err
	}
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	rec := filepath.Join(dir, info.ID+".json")
	if err := os.WriteFile(rec+".tmp", b, 0o600); err != nil {
		_ = removeDir(filepath.Join(dir, info.ID))
		return err
	}
	if err := os.Rename(rec+".tmp", rec); err != nil {
		_ = os.Remove(rec + ".tmp")
		_ = removeDir(filepath.Join(dir, info.ID))
		return err
	}
	return nil
}

// checkpointsOp lists the workspace's checkpoints.
func (w *worker) checkpointsOp(ctx context.Context, id, real string) answer {
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	list := w.checkpointList(id)
	if list == nil {
		list = []workspace.CheckpointInfo{}
	}
	return jsonAnswer(workspace.CheckpointsAnswer{Checkpoints: list, Usage: u.snapshot()})
}

// deleteCheckpointOp removes one checkpoint and gives its bytes back to the
// quota. The record goes first, so a worker killed part way leaves a folder
// with no record, which start-up removes.
func (w *worker) deleteCheckpointOp(ctx context.Context, id, real string, raw []byte) answer {
	var in workspace.CheckpointRef
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if !validCheckpointID(in.Checkpoint) {
		return refuse(workspace.ClassCheckpointNotFound, "no checkpoint %q in this workspace", in.Checkpoint)
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	info, ok := w.readCheckpoint(id, in.Checkpoint)
	if !ok {
		return jsonAnswer(workspace.DeleteCheckpointAnswer{Deleted: false, Usage: u.snapshot()})
	}
	dir := w.checkpointsOf(id)
	if err := os.Remove(filepath.Join(dir, info.ID+".json")); err != nil {
		return refuse(workspace.ClassFailed, "deleting the checkpoint: %v", err)
	}
	u.held = max(u.held-info.Bytes, 0)
	u.full.Store(u.over())
	if err := removeDir(filepath.Join(dir, info.ID)); err != nil {
		w.logger().Warn("cannot remove a deleted checkpoint's files; the next start removes them", "workspace_id", id, "err", err)
	}
	return jsonAnswer(workspace.DeleteCheckpointAnswer{Deleted: true, Usage: u.snapshot()})
}

// restoreOp puts the workspace back to a checkpoint (RFC GW §5.4). Files
// only: the workspace's kernels and its session are stopped first, and come
// back new on the next cell or command. The checkpoint's tree is copied
// beside the workspace, the workspace's folder swapped for it by two
// renames, and the old folder removed; the checkpoint stays.
func (w *worker) restoreOp(ctx context.Context, id, dir, real string, raw []byte) answer {
	var in workspace.CheckpointRef
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	info, ok := w.readCheckpoint(id, in.Checkpoint)
	if !ok {
		return refuse(workspace.ClassCheckpointNotFound, "no checkpoint %q in this workspace (checkpoints lists them)", in.Checkpoint)
	}
	// The restored files replace the live ones, so what must fit is the
	// checkpoint's size beside every checkpoint held.
	u.held = w.checkpointBytes(id)
	if info.Bytes > u.limit-u.held {
		return refuse(workspace.ClassOverQuota, "restoring needs %d bytes beside the %d its checkpoints hold, over the quota of %d; delete a checkpoint first", info.Bytes, u.held, u.limit)
	}
	tmp, err := w.tmpPath("restore")
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	if _, err := w.copyTree(ctx, filepath.Join(w.checkpointsOf(id), info.ID), tmp, u.limit-u.held); err != nil {
		if errors.Is(err, errOverBudget) {
			return refuse(workspace.ClassOverQuota, "the checkpoint's files pass the quota; nothing was changed")
		}
		return refuse(workspace.ClassFailed, "copying the checkpoint: %v", err)
	}
	// Nothing may still run in the folder being replaced: a container
	// session holds it mounted, and would go on writing into the old one;
	// a server (RFC GW P9) would go on serving it.
	kernels := w.kernels.Drop(id)
	w.servers.Drop(id)
	if err := w.engine.Forget(ctx, id); err != nil {
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "stopping the workspace's session: %v", err)
	}
	old, err := w.tmpPath("old")
	if err != nil {
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "%v", err)
	}
	if err := os.Rename(dir, old); err != nil {
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "replacing the workspace's files: %v", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if rerr := os.Rename(old, dir); rerr != nil {
			w.logger().Error("a restore could not put the workspace's folder back", "workspace_id", id, "err", rerr)
		}
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "replacing the workspace's files: %v", err)
	}
	if err := removeDir(old); err != nil {
		w.logger().Warn("cannot remove a restored workspace's old files; the next start removes them", "workspace_id", id, "err", err)
	}
	// The count is of the new folder.
	if nv, err := fsops.OpenVolumeFS(real, ""); err == nil {
		w.measureLocked(ctx, u, nv)
		nv.Close()
	} else {
		u.measured = false
	}
	return jsonAnswer(workspace.RestoreAnswer{Checkpoint: info.ID, KernelsStopped: kernels, SessionStopped: true, Usage: u.snapshot()})
}

// forkOp makes workspace id, which must not exist, as a copy of workspace
// in.From on this worker (RFC GW §5.4): of its present files, or of one of
// its checkpoints. The copy must fit the fork's own quota. The source's
// write lock is held while it is copied, so no write, restore or deletion of
// the checkpoint read happens under the copy; it is let go before the
// worker's open lock is taken (open takes that lock, then a workspace's).
func (w *worker) forkOp(ctx context.Context, id, dir string, raw []byte) answer {
	var in workspace.ForkArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if !workspace.ValidID(in.From) || in.From == id {
		return refuse(workspace.ClassInvalid, "from must name another workspace on this worker")
	}
	if in.DiskQuotaBytes < 0 || in.DiskQuotaBytes > workspace.MaxDiskQuotaBytes {
		return refuse(workspace.ClassInvalid, "disk_quota_bytes must be from 1 to %d", int64(workspace.MaxDiskQuotaBytes))
	}
	if in.Checkpoint != "" && !validCheckpointID(in.Checkpoint) {
		return refuse(workspace.ClassCheckpointNotFound, "no checkpoint %q in the workspace forked", in.Checkpoint)
	}
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() {
			return refuse(workspace.ClassFailed, "the workspace's name is taken by something that is not a directory")
		}
		// A retried fork that finished: the rename into place is its last
		// step.
		return w.forkAnswer(ctx, id, dir, workspace.ForkAnswer{Mechanism: w.mechanism()})
	}
	if a := w.atCapacity(); a.class != "" {
		return a
	}
	srcDir := filepath.Join(w.opt.Root, in.From)
	if st, err := os.Lstat(srcDir); err != nil || !st.IsDir() {
		return refuse(workspace.ClassNotFound, "the workspace forked is not on this worker")
	}
	srcReal, err := filepath.EvalSymlinks(srcDir)
	if err != nil {
		return refuse(workspace.ClassFailed, "reading the workspace forked: %v", err)
	}
	quota := in.DiskQuotaBytes
	if quota == 0 {
		quota = workspace.DefaultDiskQuotaBytes
	}
	tmp, err := w.tmpPath("fork")
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	res, a := w.copySource(ctx, in, srcReal, tmp, quota)
	if a.class != "" {
		return a
	}
	out := workspace.ForkAnswer{Bytes: res.bytes, Files: res.files, Mechanism: w.mechanism(), Skipped: res.skipped}
	if w.hostEngine() {
		real, err := filepath.EvalSymlinks(w.opt.Root)
		if err != nil {
			_ = removeDir(tmp)
			return refuse(workspace.ClassFailed, "%v", err)
		}
		out.Rewritten = rewriteVenvs(tmp, []string{srcReal, srcDir}, filepath.Join(real, id))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if a := w.atCapacity(); a.class != "" {
		_ = removeDir(tmp)
		return a
	}
	if err := w.setLimit(id, quota); err != nil {
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "recording the workspace's quota: %v", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = removeDir(tmp)
		_ = os.Remove(w.quotaFile(id))
		return refuse(workspace.ClassFailed, "placing the fork: %v", err)
	}
	return w.forkAnswer(ctx, id, dir, out)
}

// copySource copies the fork's source, under the source's write lock.
func (w *worker) copySource(ctx context.Context, in workspace.ForkArgs, srcReal, tmp string, quota int64) (copyResult, answer) {
	v, err := fsops.OpenVolumeFS(srcReal, "")
	if err != nil {
		return copyResult{}, refuse(workspace.ClassFailed, "opening the workspace forked: %v", err)
	}
	defer v.Close()
	_, release, err := w.writeLock(ctx, in.From, v)
	if err != nil {
		return copyResult{}, refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	src := srcReal
	if in.Checkpoint != "" {
		info, ok := w.readCheckpoint(in.From, in.Checkpoint)
		if !ok {
			return copyResult{}, refuse(workspace.ClassCheckpointNotFound, "no checkpoint %q in the workspace forked (checkpoints lists them)", in.Checkpoint)
		}
		if info.Bytes > quota {
			return copyResult{}, refuse(workspace.ClassOverQuota, "the checkpoint holds %d bytes, over a new workspace's quota of %d; nothing was made", info.Bytes, quota)
		}
		src = filepath.Join(w.checkpointsOf(in.From), info.ID)
	}
	res, err := w.copyTree(ctx, src, tmp, quota)
	if errors.Is(err, errOverBudget) {
		return copyResult{}, refuse(workspace.ClassOverQuota, "the workspace forked holds more than a new workspace's quota of %d bytes; nothing was made", quota)
	}
	if err != nil {
		return copyResult{}, refuse(workspace.ClassFailed, "copying the workspace: %v", err)
	}
	return res, answer{}
}

// forkAnswer measures the new workspace and answers.
func (w *worker) forkAnswer(ctx context.Context, id, dir string, out workspace.ForkAnswer) answer {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return refuse(workspace.ClassFailed, "reading the fork: %v", err)
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the fork: %v", err)
	}
	defer v.Close()
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	out.Usage = u.snapshot()
	return jsonAnswer(out)
}

// atCapacity refuses a new workspace on a worker holding its capacity.
func (w *worker) atCapacity() answer {
	held, err := w.held()
	if err != nil {
		return refuse(workspace.ClassFailed, "listing the workspaces: %v", err)
	}
	if len(held) >= w.opt.Capacity {
		return refuse(workspace.ClassAtCapacity, "this worker holds %d workspaces, its capacity", len(held))
	}
	return answer{}
}

// mechanism is the worker's checkpoint mechanism; a worker that never
// probed (a test's) copies.
func (w *worker) mechanism() string {
	if w.mech == "" {
		return workspace.MechanismCopy
	}
	return w.mech
}

// hostEngine reports whether commands run on the worker's own machine,
// where a workspace's folder is its path in the session too.
func (w *worker) hostEngine() bool {
	return w.opt.Engine == "" || w.opt.Engine == workspace.EngineHost
}

// logger is the worker's log, or one that drops everything for a worker
// built without one (a test's).
func (w *worker) logger() *slog.Logger {
	if w.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return w.log
}

// probeMechanism picks the cheapest checkpoint mechanism the root's disk
// offers (RFC GW §15 spike F): a whole-folder clone, a reflink per file, or
// a copy. Never hard links.
//
// It probes in a folder of its own in the root (the file system the
// checkpoints will be on) and removes it, so the root holds nothing new until
// a first checkpoint is taken.
func probeMechanism(root string) string {
	dir, err := os.MkdirTemp(root, probePrefix)
	if err != nil {
		return workspace.MechanismCopy
	}
	defer os.RemoveAll(dir)
	switch {
	case cloneDirSupported(dir):
		return workspace.MechanismClone
	case reflinkSupported(dir):
		return workspace.MechanismReflink
	}
	return workspace.MechanismCopy
}

// cleanCheckpoints removes what a worker killed part way left: the work in
// progress, the checkpoints of workspaces no longer held, and in a held
// workspace's folder a checkpoint without its record or a record without
// its checkpoint. Run at start, before any op.
func (w *worker) cleanCheckpoints(keep map[string]bool) {
	if ents, err := os.ReadDir(w.opt.Root); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), probePrefix) {
				_ = removeDir(filepath.Join(w.opt.Root, e.Name()))
			}
		}
	}
	root := w.checkpointsRoot()
	if err := removeDir(filepath.Join(root, checkpointTmp)); err != nil {
		w.logger().Warn("cannot clear the checkpoints' work folder", "err", err)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if name == checkpointTmp {
			continue
		}
		if !keep[name] || !e.IsDir() {
			_ = removeDir(filepath.Join(root, name))
			continue
		}
		w.cleanCheckpointsOf(name)
	}
}

func (w *worker) cleanCheckpointsOf(id string) {
	dir := w.checkpointsOf(id)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		ck := strings.TrimSuffix(strings.TrimSuffix(name, ".tmp"), ".json")
		keep := false
		if e.IsDir() {
			_, keep = w.readCheckpoint(id, name)
		} else if strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".tmp") {
			_, keep = w.readCheckpoint(id, ck)
		}
		if !keep {
			if err := removeDir(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				w.logger().Warn("cannot remove an unfinished checkpoint", "workspace_id", id, "err", err)
			}
		}
	}
}

// checkpointOps runs the checkpoint ops on workspace id.
func (w *worker) checkpointOps(ctx context.Context, id, dir, real, op string, raw []byte) answer {
	switch op {
	case workspace.OpCheckpoint:
		return w.checkpointOp(ctx, id, real, raw)
	case workspace.OpCheckpoints:
		return w.checkpointsOp(ctx, id, real)
	case workspace.OpDeleteCheckpoint:
		return w.deleteCheckpointOp(ctx, id, real, raw)
	}
	return w.restoreOp(ctx, id, dir, real, raw)
}
