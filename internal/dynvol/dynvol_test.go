package dynvol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolvedTempDir is a dynamic root with no symlink in it (on macOS t.TempDir
// sits under /var, a symlink to /private/var). symlinkedRoot builds the
// symlinked case explicitly, so it is covered on Linux too.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestProvision_DerivesUnderTheTenantSegmentAndCreates0700(t *testing.T) {
	root := resolvedTempDir(t)
	for _, tc := range []struct{ tenant, seg string }{{"", "_shared"}, {"acme", "acme"}} {
		path, created, err := Provision(root, tc.tenant, "data")
		if err != nil {
			t.Fatalf("Provision(%q): %v", tc.tenant, err)
		}
		if want := filepath.Join(root, tc.seg, "data"); path != want {
			t.Errorf("tenant %q: path = %q, want %q", tc.tenant, path, want)
		}
		if !created {
			t.Errorf("tenant %q: created = false for a fresh directory", tc.tenant)
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("tenant %q: no directory at %q: %v", tc.tenant, path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("tenant %q: mode = %o, want 700", tc.tenant, perm)
		}
		// A second call finds it there.
		if _, again, err := Provision(root, tc.tenant, "data"); err != nil || again {
			t.Errorf("tenant %q: re-provision created=%v err=%v, want false/nil", tc.tenant, again, err)
		}
	}
}

// A name that is not a single charset-clean segment never reaches MkdirAll,
// including "a/b", which would stay inside the root and so pass the fence
// alone.
func TestProvision_RefusesANameThatIsNotOneSegment(t *testing.T) {
	root := resolvedTempDir(t)
	for _, bad := range []string{"../escape", "..", "a/b", ".hidden", "_shared", "Upper", "", "x/../../y"} {
		if _, _, err := Provision(root, "acme", bad); err == nil {
			t.Errorf("Provision accepted name %q", bad)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refused names left %d entries under the root", len(entries))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); !os.IsNotExist(err) {
		t.Errorf("a traversal name created a directory beside the root (err=%v)", err)
	}
}

func TestProvision_RefusesAReservedTenant(t *testing.T) {
	root := resolvedTempDir(t)
	for _, tenant := range []string{SharedTenantSegment, EphemeralSegment} {
		if _, _, err := Provision(root, tenant, "data"); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("Provision(tenant %q) err = %v, want reserved", tenant, err)
		}
	}
}

// The fence refuses a derived path that is the root itself or leaves it.
func TestAssertInsideRoot_RefusesTheRootAndEscapes(t *testing.T) {
	root := resolvedTempDir(t)
	for _, p := range []string{root, filepath.Dir(root), filepath.Join(root, "..", "x"), "/etc"} {
		if err := AssertInsideRoot(root, p); err == nil {
			t.Errorf("AssertInsideRoot accepted %q", p)
		}
	}
	if err := AssertInsideRoot(root, filepath.Join(root, "_shared", "data")); err != nil {
		t.Errorf("AssertInsideRoot refused a path inside the root: %v", err)
	}
}

// A symlink planted at the tenant segment or at the leaf would have MkdirAll
// create — and the caller store — a directory outside the root. Provision
// refuses both, and creates nothing through the link.
func TestProvision_RefusesASymlinkBelowTheRoot(t *testing.T) {
	root := resolvedTempDir(t)
	outside := resolvedTempDir(t)

	// The leaf itself is a symlink to a directory outside the root.
	if err := os.MkdirAll(filepath.Join(root, SharedTenantSegment), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, SharedTenantSegment, "leaf")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Provision(root, "", "leaf"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Provision over a symlinked leaf: err = %v, want a symlink refusal", err)
	}

	// The tenant segment is a symlink; MkdirAll would create outside/data.
	if err := os.Symlink(outside, filepath.Join(root, "acme")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Provision(root, "acme", "data"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Provision under a symlinked tenant segment: err = %v, want a symlink refusal", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "data")); !os.IsNotExist(err) {
		t.Errorf("Provision created a directory through the symlink (err=%v)", err)
	}
}

func TestProvisionEphemeral_DerivesUnderTheRunDir(t *testing.T) {
	root := resolvedTempDir(t)
	path, err := ProvisionEphemeral(root, "run-1", "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(EphemeralRunDir(root, "run-1"), "scratch"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := ProvisionEphemeral(root, "run-1", "../x"); err == nil {
		t.Error("ProvisionEphemeral accepted a traversal name")
	}
}

// symlinkedRoot is a dynamic root configured THROUGH a symlink, as /tmp is on
// macOS (-> /private/tmp) or a symlinked NAS mount point. It returns the link
// (what the operator configures) and the real directory it points at.
func symlinkedRoot(t *testing.T) (link, target string) {
	t.Helper()
	target = resolvedTempDir(t)
	link = filepath.Join(resolvedTempDir(t), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

// A root reached through a symlink is the operator's own choice and is
// trusted: Provision creates under it, and the returned path — what the row
// stores — is spelled under the root as configured.
func TestProvision_ASymlinkedRootProvisionsUnderIt(t *testing.T) {
	link, target := symlinkedRoot(t)
	path, created, err := Provision(link, "acme", "data")
	if err != nil {
		t.Fatalf("Provision under a symlinked root: %v", err)
	}
	if want := filepath.Join(link, "acme", "data"); path != want || !created {
		t.Errorf("path = %q created = %v, want %q true", path, created, want)
	}
	if info, err := os.Stat(filepath.Join(target, "acme", "data")); err != nil || !info.IsDir() {
		t.Errorf("no directory in the target root: %v", err)
	}
	if _, again, err := Provision(link, "acme", "data"); err != nil || again {
		t.Errorf("re-provision created=%v err=%v, want false/nil", again, err)
	}
	if _, err := ProvisionEphemeral(link, "run-1", "scratch"); err != nil {
		t.Errorf("ProvisionEphemeral under a symlinked root: %v", err)
	}
}

// Both spellings of a path inside a symlinked root are inside it; the root
// itself (either spelling), its parent and an escape are not.
func TestAssertInsideRoot_ASymlinkedRootComparesTheSameForm(t *testing.T) {
	link, target := symlinkedRoot(t)
	for _, p := range []string{filepath.Join(link, "_shared", "data"), filepath.Join(target, "_shared", "data")} {
		if err := AssertInsideRoot(link, p); err != nil {
			t.Errorf("AssertInsideRoot(%q) refused a path inside the root: %v", p, err)
		}
		if got, err := UnderResolvedRoot(link, p); err != nil || got != filepath.Join(target, "_shared", "data") {
			t.Errorf("UnderResolvedRoot(%q) = %q, %v; want it under %s", p, got, err, target)
		}
	}
	for _, p := range []string{link, target, filepath.Dir(target), filepath.Join(link, "..", "x"), "/etc"} {
		if err := AssertInsideRoot(link, p); err == nil {
			t.Errorf("AssertInsideRoot accepted %q", p)
		}
	}
}

// Trusting a symlinked ROOT does not trust a symlink BELOW it: one planted at
// the tenant segment or the leaf is still refused, nothing is created through
// it, and a traversal name is still refused. (It asserts the refusal, not its
// wording: before symlinked roots worked, every create under one was refused
// as an escape, and this must have held then too.)
func TestProvision_ASymlinkedRootStillRefusesASymlinkBelowIt(t *testing.T) {
	link, target := symlinkedRoot(t)
	outside := resolvedTempDir(t)
	if err := os.Symlink(outside, filepath.Join(target, "acme")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Provision(link, "acme", "data"); err == nil {
		t.Error("Provision under a symlinked tenant segment was accepted")
	}
	if err := os.MkdirAll(filepath.Join(target, SharedTenantSegment), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, SharedTenantSegment, "leaf")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Provision(link, "", "leaf"); err == nil {
		t.Error("Provision over a symlinked leaf was accepted")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("Provision created %d entries through the symlink", len(entries))
	}
	for _, bad := range []string{"../escape", "a/b", "x/../../y"} {
		if _, _, err := Provision(link, "beta", bad); err == nil {
			t.Errorf("Provision under a symlinked root accepted name %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "escape")); !os.IsNotExist(err) {
		t.Errorf("a traversal name created a directory beside the root (err=%v)", err)
	}
}
