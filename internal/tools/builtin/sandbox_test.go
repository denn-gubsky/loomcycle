package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

// A RELATIVE tool path resolves against the sandbox ROOT, not the loomcycle
// process working directory.
//
// Fail-before: resolveInsideRoot used filepath.Abs (process-cwd-relative), so
// "sub/file.txt" looked under the test binary's cwd — outside the sandbox —
// and returned an error. This is the bug behind the code-reviewer agent's
// `Read internal/store/store.go` resolving into a stray repo-root binary
// (ENOTDIR) instead of the cloned tree under the sandbox.
func TestResolveInsideRoot_RelativeAnchoredToRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sub, "file.txt")
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks the expectation so a /var→/private/var (macOS) root matches.
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveInsideRoot(root, "sub/file.txt")
	if err != nil {
		t.Fatalf("relative path under root should resolve; got error: %v", err)
	}
	if got != wantResolved {
		t.Errorf("resolved = %q, want %q", got, wantResolved)
	}
}

// Absolute in-root paths are unchanged by the anchoring change.
func TestResolveInsideRoot_AbsoluteStillResolves(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "f.txt")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInsideRoot(root, abs); err != nil {
		t.Fatalf("absolute in-root path should resolve: %v", err)
	}
}

// Anchoring to root must NOT weaken the escape check: a relative path that
// climbs out with `..` to a real file outside the sandbox is still rejected.
func TestResolveInsideRoot_RelativeEscapeRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	// From root, ../outside/secret.txt is a REAL file outside the sandbox —
	// must be rejected by the containment check, not silently read.
	if _, err := resolveInsideRoot(root, "../outside/secret.txt"); err == nil {
		t.Error("relative path escaping the sandbox root must be rejected")
	}
}

// Write's parent-resolver anchors a relative path (whose file need not exist
// yet) to the sandbox root too — same fix, same rationale.
func TestResolveParentInsideRoot_RelativeAnchoredToRoot(t *testing.T) {
	root := t.TempDir()
	got, err := resolveParentInsideRoot(root, "newfile.txt")
	if err != nil {
		t.Fatalf("relative new-file path under root should resolve: %v", err)
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(rootResolved, "newfile.txt")
	if got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}
