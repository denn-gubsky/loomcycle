package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// symlinkedVolumeRoot is a dynamic root configured THROUGH a symlink — /tmp
// on macOS (-> /private/tmp), or a symlinked NAS mount point. It returns the
// link (what the operator configures) and the real directory behind it.
func symlinkedVolumeRoot(t *testing.T) (link, target string) {
	t.Helper()
	target = volumeRoot(t)
	link = filepath.Join(volumeRoot(t), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

// A restore onto a target whose dynamic root is a symlink provisions the
// directory in the real root, stores the path under the root as configured,
// names neither spelling of the root in a warning, and a re-restore is
// silent.
func TestVolumeDefs_RestoreOntoASymlinkedRootProvisions(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	srcLink, _ := symlinkedVolumeRoot(t)
	dstLink, dstReal := symlinkedVolumeRoot(t)
	plantVolume(t, src, srcLink, "acme", "data", "rw")

	raw := mustCapture(t, src)
	res := mustRestore(t, dst, raw, volumeOpts(dstLink))
	if c := res.Counts(); c["volume_defs"] != 1 || c["volume_dirs_created"] != 1 {
		t.Fatalf("counts %v, want 1/1 (warnings %v)", c, res.Warnings)
	}
	if b, ok := storedVolume(t, dst, "acme", "data"); !ok || b.Path != filepath.Join(dstLink, "acme", "data") {
		t.Errorf("restored as %+v (ok=%v), want the path under %s", b, ok, dstLink)
	}
	if info, err := os.Stat(filepath.Join(dstReal, "acme", "data")); err != nil || !info.IsDir() {
		t.Errorf("no directory in the directory behind the link: %v", err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, dstLink) || strings.Contains(w, dstReal) {
			t.Errorf("a warning spells out a host volume root: %s", w)
		}
	}
	again := mustRestore(t, dst, raw, volumeOpts(dstLink))
	if warningWith(again.Warnings, "volume_def") {
		t.Errorf("re-restore of the same volume warned: %v", again.Warnings)
	}
}

// A live row written while the root was configured by its real path is the
// same volume a restore derives under a symlink to that root: the restore
// leaves it standing without claiming its directory differs.
func TestVolumeDefs_ALiveRowUnderTheRealRootMatchesTheLinkedDerivation(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantVolume(t, src, volumeRoot(t), "acme", "data", "rw")
	dstLink, dstReal := symlinkedVolumeRoot(t)
	livePath := plantVolume(t, dst, dstReal, "acme", "data", "rw")

	res := mustRestore(t, dst, mustCapture(t, src), volumeOpts(dstLink))
	if c := res.Counts(); c["volume_defs"] != 0 || c["volume_dirs_created"] != 0 {
		t.Errorf("counts %v, want nothing written", c)
	}
	if b, _ := storedVolume(t, dst, "acme", "data"); b.Path != livePath {
		t.Errorf("live row became %+v, want it at %s", b, livePath)
	}
	if warningWith(res.Warnings, "volume_def acme/data") {
		t.Errorf("the restore reported the same volume as different: %v", res.Warnings)
	}
}
