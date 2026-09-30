package dynvol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolvedTempDir is a dynamic root with no symlink in it: on macOS t.TempDir
// sits under /var, a symlink to /private/var, and the fence compares against
// the resolved root.
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
