//go:build linux || darwin

package builtin

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/dynvol"
)

// The purge fence checks a derived path, then deletes it. A writer that can
// rename inside the dynamic root (a run with Bash on a static volume that
// contains it) can swap an ancestor of that path, here _ephemeral itself,
// for a symlink between the check and the delete. On code that deletes by
// the checked path string, the delete then lands wherever the link points.
const purgeRaceIterations = 1000

// purgeRace runs PurgeEphemeralRunTree for run-1 purgeRaceIterations times
// while a goroutine swaps <root>/_ephemeral for a symlink to linkTarget and
// back. victim is a directory named run-1 at linkTarget, holding a sentinel
// file; it returns how many purges deleted that sentinel.
func purgeRace(t *testing.T, root, linkTarget string) (escapes, purged int) {
	t.Helper()
	eph := filepath.Join(root, dynvol.EphemeralSegment)
	swap := eph + ".swap"
	victim := filepath.Join(linkTarget, "run-1")
	sentinel := filepath.Join(victim, "sentinel.txt")
	if err := os.Symlink(linkTarget, swap); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(eph, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exchangePaths(eph, swap); err != nil {
		t.Skipf("atomic rename exchange unsupported here: %v", err)
	}
	if err := exchangePaths(eph, swap); err != nil {
		t.Fatal(err)
	}

	// restore leaves _ephemeral the real directory.
	restore := func() {
		if fi, err := os.Lstat(eph); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := exchangePaths(eph, swap); err != nil {
				t.Fatalf("restoring %s: %v", eph, err)
			}
		}
	}
	for range purgeRaceIterations {
		// Fresh state each round: the run's tree inside, the victim outside.
		if err := os.MkdirAll(filepath.Join(eph, "run-1", "work"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(victim, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}

		var done atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				_ = exchangePaths(eph, swap)
			}
		}()
		removed, _ := PurgeEphemeralRunTree(root, "run-1", "race test")
		done.Store(true)
		wg.Wait()
		restore()

		if removed {
			purged++
		}
		if _, err := os.Stat(sentinel); os.IsNotExist(err) {
			escapes++
		}
	}
	return escapes, purged
}

// A swap to a directory outside the dynamic root must never let the purge
// delete there.
func TestPurgeEphemeralRunTree_SymlinkSwapRaceNeverDeletesOutside(t *testing.T) {
	root, outside := realDir(t), realDir(t)
	escapes, purged := purgeRace(t, root, outside)
	if escapes > 0 {
		t.Errorf("the purge deleted a directory outside the dynamic root %d times in %d purges", escapes, purgeRaceIterations)
	}
	if purged == 0 {
		t.Errorf("no purge removed the run tree in %d tries; the race never let it run", purgeRaceIterations)
	}
}

// A swap to another directory inside the dynamic root (another tenant's
// volumes) passes containment in the root, so only the per-component check
// stops the purge from deleting there.
func TestPurgeEphemeralRunTree_SymlinkSwapRaceNeverDeletesAnotherTenant(t *testing.T) {
	root := realDir(t)
	other := filepath.Join(root, dynvol.TenantSegment("tenant-b"))
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	escapes, purged := purgeRace(t, root, other)
	if escapes > 0 {
		t.Errorf("the purge deleted another tenant's directory %d times in %d purges", escapes, purgeRaceIterations)
	}
	if purged == 0 {
		t.Errorf("no purge removed the run tree in %d tries; the race never let it run", purgeRaceIterations)
	}
}
