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

// Behaviour lock for the exp7 read-path hardening (io.ReadAll over a
// LimitReader instead of a single f.Read + err.Error()=="EOF"). These pin
// the three cases that matter: a multi-page file read in full, a file larger
// than the cap bounded to exactly maxBytes, and an empty file. (Not a strict
// fail-before: a single os.File.Read does not truncate a regular file in
// practice — the hardening covers short reads on FIFOs/devices/>1 GiB reads
// and removes the fragile EOF string compare.)
func TestRead_FullReadBoundedAndEmpty(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// (1) Multi-page file (1 MiB), cap above size → read in full, intact.
	big := strings.Repeat("ABCDEFGH", 1<<17) // 1 MiB
	bigPath := filepath.Join(root, "big.txt")
	if err := os.WriteFile(bigPath, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Read{Root: root, MaxBytes: 4 << 20}
	in, _ := json.Marshal(map[string]string{"path": bigPath})
	res, _ := r.Execute(context.Background(), in)
	if res.IsError {
		t.Fatalf("big read errored: %q", res.Text)
	}
	if res.Text != big {
		t.Errorf("big file not read in full: got %d bytes, want %d", len(res.Text), len(big))
	}

	// (2) File larger than the cap → bounded to exactly maxBytes.
	rCap := &Read{Root: root, MaxBytes: 1024}
	res, _ = rCap.Execute(context.Background(), in)
	if res.IsError {
		t.Fatalf("capped read errored: %q", res.Text)
	}
	if len(res.Text) != 1024 {
		t.Errorf("capped read = %d bytes, want exactly 1024", len(res.Text))
	}
	if res.Text != big[:1024] {
		t.Errorf("capped read returned the wrong leading bytes")
	}

	// (3) Empty file → empty result, no error (the old EOF branch).
	emptyPath := filepath.Join(root, "empty.txt")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inEmpty, _ := json.Marshal(map[string]string{"path": emptyPath})
	res, _ = r.Execute(context.Background(), inEmpty)
	if res.IsError {
		t.Fatalf("empty read errored: %q", res.Text)
	}
	if res.Text != "" {
		t.Errorf("empty file Text = %q, want empty", res.Text)
	}
}
