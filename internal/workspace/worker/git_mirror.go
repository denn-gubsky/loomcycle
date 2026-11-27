package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// git_mirror.go — a mirror's bounds on the worker (RFC GW P11, the owner's
// ruling of 2026-11-26): its size, which a fetch may not take it past
// (workspace.git.mirror_max_mb, sent by the core with each fetch), and its
// end, once no workspace pins it and it has been idle
// (workspace.git.mirror_idle_s, the worker's Options.MirrorIdle).

// gitTooLarge is the class of a fetch its read bound stopped.
var gitTooLarge = gitsource.ErrTooLarge.Error()

// mirrorUsedFile is the stamp an op on a mirror touches: the sweep reads
// its time as the mirror's last use.
const mirrorUsedFile = ".loom-used"

// touchMirror marks mirror key used now. Best effort: a mirror whose stamp
// cannot be written is seen as idle since it was made, as if never used.
func (w *worker) touchMirror(key string) {
	p := filepath.Join(w.opt.Root, mirrorsDir, key, mirrorUsedFile)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		_ = os.WriteFile(p, nil, 0o600)
	}
}

// mirrorBytes is the size of mirror key's files.
func (w *worker) mirrorBytes(key string) (int64, error) {
	var n int64
	err := filepath.WalkDir(filepath.Join(w.opt.Root, mirrorsDir, key), func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err == nil {
			n += fi.Size()
		}
		return err
	})
	return n, err
}

// capFetch bounds a fetch of mirror key by max, the mirror's cap: the bytes
// the fetch may read become at most what the cap leaves. A pack over that
// is refused by its check before anything is stored. It reports whether
// the cap is what bounds the fetch, and refuses a mirror already at it.
func (w *worker) capFetch(key string, max int64, n *gitrepo.Network) (bool, answer) {
	if max <= 0 {
		return false, answer{}
	}
	size, err := w.mirrorBytes(key)
	if err != nil {
		return false, refuse(workspace.ClassFailed, "measuring the mirror: %v", err)
	}
	room := max - size
	if room <= 0 {
		return false, mirrorFull()
	}
	if n.Guard.MaxReadBytes <= 0 || room < n.Guard.MaxReadBytes {
		n.Guard.MaxReadBytes = room
		return true, answer{}
	}
	return false, answer{}
}

func mirrorFull() answer {
	return refuse(workspace.ClassMirrorTooLarge, "the fetch would take the repository's mirror past workspace.git.mirror_max_mb")
}

// sweepMirrors removes the mirrors no workspace pins that have been idle
// for Options.MirrorIdle; zero removes none. A mirror an op holds is
// skipped, never waited for.
func (w *worker) sweepMirrors(now time.Time) {
	if w.opt.MirrorIdle <= 0 {
		return
	}
	ents, err := os.ReadDir(filepath.Join(w.opt.Root, mirrorsDir))
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !gitrepo.ValidMirrorKey(e.Name()) {
			continue
		}
		w.sweepMirror(e.Name(), now)
	}
}

func (w *worker) sweepMirror(key string, now time.Time) {
	unlock, ok := w.tryLockMirror(key)
	if !ok {
		return
	}
	defer unlock()
	dir := filepath.Join(w.opt.Root, mirrorsDir, key)
	used := time.Time{}
	if fi, err := os.Stat(filepath.Join(dir, mirrorUsedFile)); err == nil {
		used = fi.ModTime()
	} else if fi, err := os.Stat(dir); err == nil {
		used = fi.ModTime()
	}
	if now.Sub(used) < w.opt.MirrorIdle {
		return
	}
	fs, done, err := w.mirrorFS(key)
	if err != nil {
		return
	}
	m, err := gitrepo.OpenMirror(fs)
	var pinned []string
	if err == nil {
		pinned, err = m.Pinned()
	}
	done()
	if err != nil || len(pinned) > 0 {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		w.log.Warn("git: removing an idle mirror failed", "mirror", key, "err", err)
		return
	}
	w.log.Info("git: removed an idle mirror no workspace pins", "mirror", key)
}

// tryLockMirror takes mirror key's lock if it is free.
func (w *worker) tryLockMirror(key string) (func(), bool) {
	w.mirrorMu.Lock()
	if w.mirrorLocks == nil {
		w.mirrorLocks = map[string]chan struct{}{}
	}
	ch, ok := w.mirrorLocks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		w.mirrorLocks[key] = ch
	}
	w.mirrorMu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

// mirrorSweepEvery is how often the sweep runs: a quarter of the idle
// bound, between a minute and ten.
func mirrorSweepEvery(idle time.Duration) time.Duration {
	return min(max(idle/4, time.Minute), 10*time.Minute)
}

// runMirrorSweep sweeps the mirrors until ctx ends.
func (w *worker) runMirrorSweep(ctx context.Context) {
	if w.opt.MirrorIdle <= 0 {
		return
	}
	safego.Go(ctx, "workspace.worker.mirror-sweep", func() {
		t := time.NewTicker(mirrorSweepEvery(w.opt.MirrorIdle))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				safego.Run(ctx, "workspace.worker.mirror-sweep.tick", func() { w.sweepMirrors(now) })
			}
		}
	})
}
