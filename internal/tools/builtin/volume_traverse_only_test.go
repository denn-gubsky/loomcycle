//go:build unix

package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file tools open every directory on a path through os.Root, which opens
// it for reading. A directory a process may traverse but not list (mode 0100,
// or a 0300 drop-box) therefore refuses the call, where opening by path
// string only needed search permission. These tests pin that trade-off (RFC
// ES #1): containment that cannot be raced, at the cost of such directories.
func traverseOnlyDir(t *testing.T, mode os.FileMode) (root, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root = realDir(t)
	dir = filepath.Join(root, "x")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("behind a traverse-only directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return root, dir
}

func TestRead_TraverseOnlyDirectoryIsRefused(t *testing.T) {
	root, _ := traverseOnlyDir(t, 0o100)
	res := swapCall(t, &Read{}, root, map[string]any{"path": "x/f.txt"})
	if !res.IsError || !strings.Contains(res.Text, "permission denied") {
		t.Errorf("Read through a 0100 directory = %q (error=%v), want a permission-denied refusal", res.Text, res.IsError)
	}
}

func TestWrite_DropBoxDirectoryIsRefused(t *testing.T) {
	root, dir := traverseOnlyDir(t, 0o300)
	res := swapCall(t, &Write{}, root, map[string]any{"path": "x/new.txt", "content": "dropped"})
	if !res.IsError || !strings.Contains(res.Text, "permission denied") {
		t.Errorf("Write into a 0300 directory = %q (error=%v), want a permission-denied refusal", res.Text, res.IsError)
	}
	_ = os.Chmod(dir, 0o700)
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Error("the refused Write still created the file")
	}
}
