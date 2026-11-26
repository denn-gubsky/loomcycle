package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// quotaSuffix names the file beside a workspace's directory, in the
// worker's root, that holds its disk quota: <id>.quota. Beside the
// directory and so out of the workspace's reach (a command could otherwise
// raise its own quota, and the container engine mounts only the
// directory). A file, so held() and anything else that counts the root's
// directories as workspaces never sees it.
const quotaSuffix = ".quota"

func (w *worker) quotaFile(id string) string { return filepath.Join(w.opt.Root, id+quotaSuffix) }

// defaultMeasureBudget bounds the walk that counts a workspace's bytes. Past
// it the count is what the walk reached, marked as an estimate.
const defaultMeasureBudget = 10 * time.Second

// usage is one workspace's quota state on this worker, for this process's
// life: made at the first op that needs it, measured at its first write.
type usage struct {
	// lock is the write lock: every op that changes the workspace's files
	// holds it from its check to its last write, so the count is exact
	// between them. A channel, so a waiting op gives up with its context.
	lock chan struct{}
	// id is the workspace's.
	id string
	// The fields below are read and written holding lock.
	limit int64
	used  int64
	// held is what the workspace's checkpoints count (RFC GW P6): their
	// recorded sizes, against the same limit as used.
	held     int64
	measured bool
	estimate bool
	// full is set when a measurement finds the workspace over its quota (a
	// command wrote past it). Read without the lock by exec's check.
	full atomic.Bool
}

// quotas is the worker's map of usage, keyed by workspace id.
type quotas struct {
	mu sync.Mutex
	m  map[string]*usage
}

func (q *quotas) get(id string, limit func() int64) *usage {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.m == nil {
		q.m = map[string]*usage{}
	}
	u := q.m[id]
	if u == nil {
		u = &usage{id: id, lock: make(chan struct{}, 1), limit: limit()}
		q.m[id] = u
	}
	return u
}

// peek is id's usage when one exists, without making it.
func (q *quotas) peek(id string) *usage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.m[id]
}

func (q *quotas) forget(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.m, id)
}

// setLimit records a quota an open named, on disk and in a usage already
// made.
func (w *worker) setLimit(id string, limit int64) error {
	if _, ok := w.boundRoot(id); ok {
		// A root has no quota (limitOf); none is recorded on it.
		return nil
	}
	tmp := w.quotaFile(id) + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(limit, 10)), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, w.quotaFile(id)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if u := w.quotas.peek(id); u != nil {
		u.lock <- struct{}{}
		u.limit = limit
		u.full.Store(u.measured && u.over())
		<-u.lock
	}
	return nil
}

// limitOf is id's quota as its open recorded it; a workspace opened with
// none (or before quotas existed) has the default.
func (w *worker) limitOf(id string) int64 {
	if _, ok := w.boundRoot(id); ok {
		// A root has no disk quota (RFC GW §4.5): the directory is the
		// user's own.
		return workspace.MaxDiskQuotaBytes
	}
	b, err := os.ReadFile(w.quotaFile(id))
	if err != nil {
		return workspace.DefaultDiskQuotaBytes
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n <= 0 || n > workspace.MaxDiskQuotaBytes {
		return workspace.DefaultDiskQuotaBytes
	}
	return n
}

// dropQuota forgets a removed workspace's quota.
func (w *worker) dropQuota(id string) {
	w.quotas.forget(id)
	_ = os.Remove(w.quotaFile(id))
}

func (w *worker) usageOf(id string) *usage {
	return w.quotas.get(id, func() int64 { return w.limitOf(id) })
}

// writeLock takes id's write lock, measuring the workspace the first time.
// The caller calls the returned release.
func (w *worker) writeLock(ctx context.Context, id string, v *fsops.VolumeFS) (*usage, func(), error) {
	u := w.usageOf(id)
	select {
	case u.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-u.lock }
	if !u.measured {
		w.measureLocked(ctx, u, v)
	}
	return u, release, nil
}

// measureLocked counts the workspace's bytes and sets full from the count.
// The caller holds the write lock.
func (w *worker) measureLocked(ctx context.Context, u *usage, v *fsops.VolumeFS) {
	if _, ok := w.boundRoot(u.id); ok {
		// A root is not counted: it has no quota, and a walk of a user's
		// whole directory would be work for nothing.
		u.used, u.held, u.estimate, u.measured = 0, 0, false, true
		u.full.Store(false)
		return
	}
	budget := w.opt.MeasureBudget
	if budget <= 0 {
		budget = defaultMeasureBudget
	}
	n, complete := treeSize(ctx, v, ".", time.Now().Add(budget))
	u.used, u.estimate, u.measured = n, !complete, true
	// The checkpoints are read from their records each time: they are
	// written by this worker only, and a few small files.
	u.held = w.checkpointBytes(u.id)
	u.full.Store(u.over())
}

// settleLocked sets full from the count after a change; a count that is an
// estimate, or a workspace marked full, is measured again, since only a
// fresh walk can say it is back under.
func (w *worker) settleLocked(ctx context.Context, u *usage, v *fsops.VolumeFS) {
	if u.estimate || u.full.Load() {
		w.measureLocked(ctx, u, v)
		return
	}
	u.full.Store(u.over())
}

// over reports whether the files and the checkpoints together pass the
// quota. The caller holds the lock.
func (u *usage) over() bool { return u.used+u.held > u.limit }

func (u *usage) snapshot() workspace.Usage {
	return workspace.Usage{UsedBytes: u.used, QuotaBytes: u.limit, Estimate: u.estimate, CheckpointBytes: u.held}
}

// admit refuses a write of need more bytes that the quota cannot hold, and
// any write while the workspace is marked full. The caller holds the lock.
func (u *usage) admit(need int64) *answer {
	about := ""
	if u.estimate {
		about = " (at least: the count is an estimate)"
	}
	held := ""
	if u.held > 0 {
		held = fmt.Sprintf(" and its checkpoints %d", u.held)
	}
	if u.full.Load() {
		return refusal(workspace.ClassOverQuota, "the workspace holds %d bytes%s%s, over its quota of %d; remove files or checkpoints to go on", u.used, about, held, u.limit)
	}
	if need > u.limit-u.used-u.held {
		return refusal(workspace.ClassOverQuota, "the workspace's quota is %d bytes and it holds %d%s%s; this write needs %d more", u.limit, u.used, about, held, need)
	}
	return nil
}

// treeSize sums the sizes of the regular files at and under rel, following
// no link, until deadline; complete is false when it stopped early.
func treeSize(ctx context.Context, v *fsops.VolumeFS, rel string, deadline time.Time) (n int64, complete bool) {
	parent, err := openDir(v, filepath.Dir(rel), false)
	if err != nil {
		return 0, true
	}
	defer parent.Close()
	base := filepath.Base(rel)
	if rel == "." {
		base = "."
	}
	fi, err := parent.Lstat(base)
	if err != nil {
		return 0, true
	}
	if !fi.IsDir() {
		return entrySize(fi), true
	}
	dir, err := v.Step(parent, base)
	if err != nil {
		return 0, true
	}
	defer dir.Close()
	complete = true
	walkTree(v, dir, "", func(_ *os.Root, _ string, fi os.FileInfo) bool {
		if ctx.Err() != nil || time.Now().After(deadline) {
			complete = false
			return false
		}
		n += entrySize(fi)
		return true
	}, nil)
	return n, complete
}

// walkTree calls fn for every entry under dir, depth first in lexical order,
// with the entry's path relative to dir and its Lstat. It descends into a
// folder through fsops's checked step, so a link is reported, never
// followed, and only when descend (nil: always) allows it. fn returns false
// to stop the walk.
func walkTree(v *fsops.VolumeFS, dir *os.Root, prefix string, fn func(parent *os.Root, rel string, fi os.FileInfo) bool, descend func(rel string) bool) bool {
	f, err := dir.Open(".")
	if err != nil {
		return true
	}
	names, _ := f.Readdirnames(-1)
	f.Close()
	slices.Sort(names)
	for _, name := range names {
		fi, err := dir.Lstat(name)
		if err != nil {
			continue
		}
		p := filepath.Join(prefix, name)
		if !fn(dir, p, fi) {
			return false
		}
		if !fi.IsDir() || (descend != nil && !descend(p)) {
			continue
		}
		sub, err := v.Step(dir, name)
		if err != nil {
			continue
		}
		ok := walkTree(v, sub, p, fn, descend)
		sub.Close()
		if !ok {
			return false
		}
	}
	return true
}

// cleanWork removes what a worker killed mid-op left in its workspaces: the
// work folder (put's temporary files, staging folders) of each, and the
// quota files of workspaces no longer there. Run at start, before any op.
func (w *worker) cleanWork() {
	held, err := w.held()
	if err != nil {
		w.log.Warn("cannot list the workspaces to clean their work folders", "err", err)
		return
	}
	keep := map[string]bool{}
	defer func() { w.kernels.Clean(keep); w.cleanCheckpoints(keep); w.cleanTemplates() }()
	for _, id := range held {
		keep[id] = true
		if _, ok := w.boundRoot(id); ok {
			// A root is the user's folder: nothing in it is deleted, a
			// folder of theirs named like the work folder included
			// (D-P13-18). What a killed put left there stays.
			continue
		}
		r, err := os.OpenRoot(filepath.Join(w.opt.Root, id))
		if err != nil {
			continue
		}
		// Through the workspace's own root, so a link put where the work
		// folder was is removed as a link, never followed.
		if err := r.RemoveAll(workDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			w.log.Warn("cannot clean a workspace's work folder", "workspace_id", id, "err", err)
		}
		r.Close()
	}
	ents, err := os.ReadDir(w.opt.Root)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := strings.TrimSuffix(e.Name(), ".tmp")
		id, ok := strings.CutSuffix(name, quotaSuffix)
		if ok && !e.IsDir() && workspace.ValidID(id) && (!keep[id] || name != e.Name()) {
			_ = os.Remove(filepath.Join(w.opt.Root, e.Name()))
		}
	}
}
