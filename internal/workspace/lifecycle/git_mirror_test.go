package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// workspace.git.mirror_max_mb (the owner's ruling, P11 PR 10): a fetch that
// would take a mirror past its cap is refused, and leaves the mirror as it
// was: its refs and its packs.
func TestGitFetch_AFetchPastTheMirrorCapIsRefusedAndLeavesTheMirror(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row := openGitWS(t, f, forge, "app", "", false)
	mirror := filepath.Join(f.h.Root, ".loomcycle-mirrors", row.Git.Mirror)
	size := dirBytes(t, mirror)
	f.m.SetGitMirrorMax(size + 4<<10)
	big := make([]byte, 256<<10) // random: it does not compress
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	forge.commit(t, "main", "big.bin", hex.EncodeToString(big))
	mainBefore := gitRun(t, mirror, "--git-dir=.", "rev-parse", "refs/heads/main")
	packsBefore := packCount(t, mirror)
	_, err := f.m.GitFetch(context.Background(), row, gitNet(forgeTokenA))
	if dispatch.ClassOf(err) != "mirror_too_large" {
		t.Fatalf("fetch past the cap = %v, want mirror_too_large", err)
	}
	if got := gitRun(t, mirror, "--git-dir=.", "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Fatalf("a refused fetch moved main to %s", got)
	}
	if n := packCount(t, mirror); n != packsBefore {
		t.Fatalf("a refused fetch left %d packs, were %d", n, packsBefore)
	}
	// Under the cap again, the same fetch goes through.
	f.m.SetGitMirrorMax(0)
	if _, err := f.m.GitFetch(context.Background(), row, gitNet(forgeTokenA)); err != nil {
		t.Fatalf("fetch with no cap: %v", err)
	}
}

func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func packCount(t *testing.T, mirror string) int {
	t.Helper()
	ents, err := filepath.Glob(filepath.Join(mirror, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}
