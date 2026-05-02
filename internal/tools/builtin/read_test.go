package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: with no Root configured, Read must refuse rather than open
// any path the process can reach.
func TestReadRefusesEmptyRoot(t *testing.T) {
	r := &Read{}
	res, err := r.Execute(context.Background(), json.RawMessage(`{"path":"/etc/hosts"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true when Root is empty")
	}
	if !strings.Contains(res.Text, "sandbox") {
		t.Errorf("error text should mention sandbox; got %q", res.Text)
	}
}

// Regression: a symlink inside the sandbox that points outside must be
// rejected. The fix is to EvalSymlinks the *target* path, not just the root.
//
// On macOS /var → /private/var as a symlink, which would make the unfixed
// code reject every path under a TempDir for the wrong reason. We resolve
// the root explicitly to neutralise that quirk so this test specifically
// exercises the symlink-target TOCTOU.
func TestReadRejectsSymlinkEscapingRoot(t *testing.T) {
	rootRaw := t.TempDir()
	outsideRaw := t.TempDir()
	root, err := filepath.EvalSymlinks(rootRaw)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(outsideRaw)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("CLASSIFIED"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Symlink inside root → file outside root.
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	r := &Read{Root: root}
	input, _ := json.Marshal(map[string]string{"path": link})
	res, err := r.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("symlink to outside-sandbox file should error; got Text=%q", res.Text)
	}
	if strings.Contains(res.Text, "CLASSIFIED") {
		t.Errorf("file contents leaked through symlink: %q", res.Text)
	}
}

// Regression: a path that escapes via `..` segments must be rejected. The
// previous string check `rel[:3] == "../"` would not have matched on
// Windows (`..\foo`); the fixed code uses filepath.Separator so both work.
func TestReadRejectsParentTraversal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &Read{Root: root}

	// Construct an absolute path one level above root.
	parent := filepath.Dir(root)
	probe := filepath.Join(parent, "anything.txt")

	input, _ := json.Marshal(map[string]string{"path": probe})
	res, err := r.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("path outside root should error; got Text=%q", res.Text)
	}
	if !strings.Contains(res.Text, "escapes sandbox") && !strings.Contains(res.Text, "no such file") {
		// The error may be either the explicit sandbox rejection or a
		// non-existence error from EvalSymlinks (if probe doesn't exist) —
		// both are correct refusals, neither leaks anything.
		t.Errorf("unexpected error text: %q", res.Text)
	}
}

// Sanity: a file inside the sandbox is read correctly.
func TestReadAllowsInsideSandbox(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "ok.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Read{Root: root}
	input, _ := json.Marshal(map[string]string{"path": target})
	res, err := r.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("expected success; got %q", res.Text)
	}
	if res.Text != "hello" {
		t.Errorf("Text = %q, want %q", res.Text, "hello")
	}
}
