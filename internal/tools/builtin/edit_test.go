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

func TestEditRefusesNoVolume(t *testing.T) {
	e := &Edit{}
	res, err := e.Execute(context.Background(), json.RawMessage(`{"path":"/tmp/x","old_string":"a","new_string":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "no filesystem volume available") {
		t.Fatalf("expected no-volume refusal, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

func TestEditRejectsIdenticalOldAndNew(t *testing.T) {
	// old==new is checked before the volume resolves, so this needs no binding.
	e := &Edit{}
	body, _ := json.Marshal(map[string]string{"path": "/x", "old_string": "same", "new_string": "same"})
	res, err := e.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected error for old==new, got %q", res.Text)
	}
}

func TestEditOldStringNotFound(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := &Edit{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "old_string": "missing", "new_string": "x"})
	res, err := e.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not found") {
		t.Errorf("expected not-found error, got %q", res.Text)
	}
}

// Mirrors Claude Code's Edit semantics: ambiguous old_string is an error
// the model can fix by widening context. Asserting it forces the model
// to think instead of overwriting all matches by accident.
func TestEditRejectsAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("foo foo foo"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := &Edit{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "old_string": "foo", "new_string": "bar"})
	res, err := e.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "occurs 3 times") {
		t.Errorf("expected ambiguity error mentioning count, got %q", res.Text)
	}
	// Confirm file untouched.
	got, _ := os.ReadFile(target)
	if string(got) != "foo foo foo" {
		t.Errorf("ambiguous edit modified file: %q", string(got))
	}
}

func TestEditSingleReplacement(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := &Edit{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "old_string": "world", "new_string": "earth"})
	res, err := e.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "hello earth" {
		t.Errorf("after edit: %q, want %q", string(got), "hello earth")
	}
}

func TestEditReplaceAll(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("foo foo foo"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := &Edit{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]any{"path": target, "old_string": "foo", "new_string": "bar", "replace_all": true})
	res, err := e.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "bar bar bar" {
		t.Errorf("after replace_all: %q, want %q", string(got), "bar bar bar")
	}
	if !strings.Contains(res.Text, "3 replacements") {
		t.Errorf("expected count in result text, got %q", res.Text)
	}
}

func TestEditRejectsSymlinkEscape(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("CLASSIFIED"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	e := &Edit{}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": link, "old_string": "CLASSIFIED", "new_string": "OPEN"})
	res, err := e.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("symlink to outside-sandbox file must be rejected; got %q", res.Text)
	}
	got, _ := os.ReadFile(secret)
	if string(got) != "CLASSIFIED" {
		t.Errorf("symlinked file was modified: %q", string(got))
	}
}

func TestEditRejectsOversizedFile(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "big.txt")
	if err := os.WriteFile(target, []byte("0123456789ABCDEF"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := &Edit{MaxBytes: 8}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	body, _ := json.Marshal(map[string]string{"path": target, "old_string": "0", "new_string": "X"})
	res, err := e.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "exceeds") {
		t.Errorf("expected size-bound error, got %q", res.Text)
	}
}
