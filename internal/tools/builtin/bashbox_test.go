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

// roCtx attaches a single READ-ONLY default volume rooted at root.
func roCtx(root string) context.Context {
	return ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true, ReadOnly: true})
}

func runBashbox(t *testing.T, b *Bashbox, ctx context.Context, command string) tools.Result {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"command": command})
	res, err := b.Execute(ctx, body)
	if err != nil {
		t.Fatalf("Execute returned a hard error: %v", err)
	}
	return res
}

// The Enabled gate refuses every call when false — the single security flag.
func TestBashbox_RefusesWhenNotEnabled(t *testing.T) {
	b := &Bashbox{}
	res, err := b.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not enabled") {
		t.Errorf("expected not-enabled refusal, got %q", res.Text)
	}
}

// THE headline property — fail-before vs Bash's refusal. Bash REFUSES a
// read-only volume because it cannot enforce ro (bash.go:92, needWrite=true).
// Bashbox instead HONORS ro: a write inside the sandbox SUCCEEDS (visible to
// the same run via the in-RAM overlay) but the host file is NEVER created.
// This is the guarantee that lifts CLAUDE.md rule #7 for the Bashbox path.
func TestBashbox_RoVolume_DiscardsWrites(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &Bashbox{Enabled: true}

	res := runBashbox(t, b, roCtx(root), "echo hacked > pwned.txt; cat pwned.txt")
	if res.IsError {
		t.Fatalf("ro write should succeed in-sandbox, not error: %q", res.Text)
	}
	// The write is visible to the same run (proves it actually happened,
	// not that writes silently no-op).
	if !strings.Contains(res.Text, "hacked") {
		t.Errorf("in-sandbox read of the written file should show its content; got %q", res.Text)
	}
	// ...but the host tree is untouched — the overlay was discarded.
	if _, statErr := os.Stat(filepath.Join(root, "pwned.txt")); !os.IsNotExist(statErr) {
		t.Errorf("read-only volume leaked a write to the host: %v", statErr)
	}
}

// Positive control: a rw volume DOES persist to the host. Without this, the
// ro test above could pass simply because writes never work at all.
func TestBashbox_RwVolume_PersistsWrites(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &Bashbox{Enabled: true}

	res := runBashbox(t, b, bashCtx(root), "echo persisted > kept.txt")
	if res.IsError {
		t.Fatalf("rw write failed: %q", res.Text)
	}
	got, readErr := os.ReadFile(filepath.Join(root, "kept.txt"))
	if readErr != nil {
		t.Fatalf("rw volume did not persist the write to the host: %v", readErr)
	}
	if strings.TrimSpace(string(got)) != "persisted" {
		t.Errorf("host file content = %q, want %q", string(got), "persisted")
	}
}

// No host filesystem escape: a secret sitting OUTSIDE the volume, addressed by
// its real absolute host path, is unreachable. The virtual FS is rooted at the
// mounted volume — there is no absolute-path back door (the property Bash,
// running a real /bin/sh, cannot provide).
func TestBashbox_NoHostFilesystemEscape(t *testing.T) {
	volRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secretDir, err := filepath.EvalSymlinks(t.TempDir()) // a SIBLING dir, not under volRoot
	if err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Bashbox{Enabled: true}
	res := runBashbox(t, b, bashCtx(volRoot), "cat "+secretPath)
	if strings.Contains(res.Text, "TOPSECRET") {
		t.Fatalf("sandbox escaped to a host file outside the volume: %q", res.Text)
	}
	if !res.IsError {
		t.Errorf("reading an out-of-sandbox absolute path should fail; got %q", res.Text)
	}
}

// No host shell-out: an unknown command (git is not a gbash command) is
// refused with a non-zero exit rather than falling through to a host binary.
func TestBashbox_UnknownCommandRefused(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true}
	res := runBashbox(t, b, bashCtx(root), "git status")
	if !res.IsError {
		t.Errorf("unknown command should be refused (no host shell-out); got %q", res.Text)
	}
}

// No network by default: curl (or any egress) is refused — v1 exposes no
// network at all.
func TestBashbox_NetworkBlockedByDefault(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true}
	res := runBashbox(t, b, bashCtx(root), "curl https://example.com")
	if !res.IsError {
		t.Errorf("network egress should be blocked by default; got %q", res.Text)
	}
	if strings.Contains(strings.ToLower(res.Text), "example domain") {
		t.Errorf("curl appears to have reached the network: %q", res.Text)
	}
}

// Output cap: a file larger than MaxOutputBytes is truncated with a marker so
// a runaway command can't flood the model's context.
func TestBashbox_ExecutionBudget_Truncates(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	big := strings.Repeat("y\n", 5000) // ~10 KiB
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Bashbox{Enabled: true, MaxOutputBytes: 100}
	res := runBashbox(t, b, bashCtx(root), "cat big.txt")
	if !strings.Contains(res.Text, "[output truncated at 100 bytes]") {
		t.Errorf("missing truncation marker; len=%d text=%q", len(res.Text), res.Text)
	}
}

// The opt-in contrib commands (awk, jq) are registered and runnable — the
// coverage spike found these are the two highest-value gaps in gbash's core
// builtins, so Bashbox bundles them.
func TestBashbox_ContribCommandsRegistered(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "cols.txt"), []byte("a 1\nb 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Bashbox{Enabled: true}

	awkRes := runBashbox(t, b, bashCtx(root), "awk '{s+=$2} END{print s}' cols.txt")
	if awkRes.IsError || strings.TrimSpace(awkRes.Text) != "3" {
		t.Errorf("awk contrib not working: err=%v out=%q", awkRes.IsError, awkRes.Text)
	}
	jqRes := runBashbox(t, b, bashCtx(root), `echo '{"a":7}' | jq .a`)
	if jqRes.IsError || strings.TrimSpace(jqRes.Text) != "7" {
		t.Errorf("jq contrib not working: err=%v out=%q", jqRes.IsError, jqRes.Text)
	}
}

// Non-zero exit is reported as IsError but the output is preserved (parity
// with the Bash tool) so the model can self-correct.
func TestBashbox_NonZeroExitSurfacesAsError(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true}
	res := runBashbox(t, b, bashCtx(root), "echo something; exit 7")
	if !res.IsError {
		t.Errorf("expected IsError on non-zero exit; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "something") {
		t.Errorf("output not preserved on error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[exit: 7]") {
		t.Errorf("exit marker missing: %q", res.Text)
	}
}
