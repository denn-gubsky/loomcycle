package worker

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
)

// workspace.git.mirror_idle_s (the owner's ruling, P11 PR 10): a mirror no
// workspace pins is removed once it has been idle that long; a pinned one,
// a recently used one and one an op holds are kept.
func TestSweepMirrors_RemovesOnlyAnIdleUnpinnedMirror(t *testing.T) {
	root := t.TempDir()
	w := &worker{opt: Options{Root: root, MirrorIdle: time.Hour}, log: slog.New(slog.DiscardHandler)}
	mk := func(key string, pinned bool, used time.Time) string {
		t.Helper()
		fs, done, err := w.mirrorFS(key)
		if err != nil {
			t.Fatal(err)
		}
		defer done()
		m, err := gitrepo.OpenMirror(fs)
		if err != nil {
			t.Fatal(err)
		}
		if pinned {
			if err := m.Pin("ws_0123456789abcdef01234567", "base", plumbing.NewHash("1111111111111111111111111111111111111111")); err != nil {
				t.Fatal(err)
			}
		}
		dir := filepath.Join(root, mirrorsDir, key)
		w.touchMirror(key)
		if err := os.Chtimes(filepath.Join(dir, mirrorUsedFile), used, used); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	now := time.Now()
	idle := mk("0000000000000000000000000000000a", false, now.Add(-2*time.Hour))
	pinned := mk("0000000000000000000000000000000b", true, now.Add(-2*time.Hour))
	recent := mk("0000000000000000000000000000000c", false, now.Add(-time.Minute))
	held := mk("0000000000000000000000000000000d", false, now.Add(-2*time.Hour))
	unlock := w.lockMirror("0000000000000000000000000000000d")
	w.sweepMirrors(now)
	unlock()
	for dir, want := range map[string]bool{idle: false, pinned: true, recent: true, held: true} {
		_, err := os.Stat(dir)
		if got := err == nil; got != want {
			t.Errorf("%s: kept %v, want %v", filepath.Base(dir), got, want)
		}
	}
	// With no idle bound set, nothing is collected.
	w.opt.MirrorIdle = 0
	w.sweepMirrors(now.Add(48 * time.Hour))
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("a sweep with no idle bound removed a mirror")
	}
}
