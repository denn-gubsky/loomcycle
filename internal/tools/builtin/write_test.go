package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Mirrors TestReadRefusesNoVolume: no volume bound → no writes regardless of
// input (RFC AH Phase 3 sandbox-by-default).
func TestWriteRefusesNoVolume(t *testing.T) {
	w := &Write{}
	res, err := w.Execute(context.Background(), json.RawMessage(`{"path":"/tmp/x","content":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "no filesystem volume available") {
		t.Fatalf("expected no-volume refusal, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

func TestWriteAllowsInsideSandbox(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "out.txt")

	w := &Write{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "content": "hello world"})
	res, err := w.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", res.Text)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Errorf("file content = %q, want %q", string(got), "hello world")
	}
}

// Same family as TestReadRejectsParentTraversal: writing above the sandbox
// must fail. Critically, the rejection must NOT create the target file
// (otherwise a sandbox bypass leaves observable state).
func TestWriteRejectsParentTraversal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(root)
	probe := filepath.Join(parent, "anywhere.txt")

	w := &Write{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": probe, "content": "x"})
	res, err := w.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("traversal must be rejected; got Text=%q", res.Text)
	}
	if _, err := os.Stat(probe); err == nil {
		t.Errorf("rejected write still created file at %s", probe)
		os.Remove(probe)
	}
}

// Same TOCTOU shape as Read's symlink test, but in the write direction:
// a symlink whose parent directory escapes the sandbox must be rejected.
// We construct: root/sub → outside; writing to root/sub/x.txt should fail
// because the resolved parent is `outside`.
func TestWriteRejectsSymlinkedParentEscape(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "sub")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(link, "x.txt")

	w := &Write{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "content": "leaked"})
	res, err := w.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("symlinked-parent escape must be rejected; got %q", res.Text)
	}
	// Confirm nothing landed outside the sandbox.
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Errorf("write leaked to outside-sandbox file")
	}
}

// Atomicity: a successful write replaces an existing file in one step.
// We can't directly observe non-atomicity in a unit test, but we can
// verify the existing file's prior content is fully replaced (no
// truncated/partial content survives) — that's the promise the model
// reads as "atomic-ish".
func TestWriteOverwritesExistingFile(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("OLD CONTENT THAT IS MUCH LONGER"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &Write{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "content": "new"})
	res, err := w.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", res.Text)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Errorf("after overwrite, content = %q, want %q", string(got), "new")
	}
}

// Bound enforcement: oversized content is refused before any write happens.
func TestWriteRefusesOversizedContent(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "big.txt")

	w := &Write{MaxBytes: 8}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "content": "this is way more than eight bytes"})
	res, err := w.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "exceeds") {
		t.Errorf("expected size-bound rejection, got %q", res.Text)
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("oversized rejection still created the file")
	}
}
