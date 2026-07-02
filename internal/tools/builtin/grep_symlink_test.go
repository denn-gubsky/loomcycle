package builtin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGrepWalk_DoesNotFollowSymlinkOutsideRoot is the regression for the Grep
// sandbox escape: grepWalk opened each walked entry with os.Open WITHOUT
// re-resolving symlinks/containment, so an in-volume symlink pointing outside
// the volume (planted via a rw Bash/Bashbox, or pre-existing in a mounted dir)
// was followed and its CONTENTS returned — unlike Read/Edit, which gate every
// open on resolveInsideRoot. A "content" grep over such a volume leaked
// out-of-volume file contents under the innocuous in-tree symlink name.
func TestGrepWalk_DoesNotFollowSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	const secret = "SECRET_TOKEN_a1b2c3"
	// A secret file OUTSIDE the volume, also containing the search pattern.
	if err := os.WriteFile(filepath.Join(outside, "creds"), []byte("FINDME "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A benign in-volume file that also matches — proves grep still works.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("FINDME inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An in-volume symlink pointing OUT of the volume at the secret file.
	if err := os.Symlink(filepath.Join(outside, "creds"), filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	re := regexp.MustCompile("FINDME")
	out, err := grepWalk(root, root, re, "", "content", 100, grepDefaultMaxOutputBytes, 0, 0)
	if err != nil {
		t.Fatalf("grepWalk: %v", err)
	}
	// The escape must NOT leak the out-of-volume file's contents.
	if strings.Contains(out, secret) {
		t.Fatalf("Grep leaked out-of-volume contents via an in-volume symlink:\n%s", out)
	}
	// ...but the legitimate in-volume match is still returned (grep works).
	if !strings.Contains(out, "inside") {
		t.Fatalf("Grep dropped a legitimate in-volume match:\n%s", out)
	}
}
