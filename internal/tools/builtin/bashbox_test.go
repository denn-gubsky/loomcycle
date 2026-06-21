package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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

// Escape vector #1 — `..` traversal out of the volume into a sibling host
// dir is refused in BOTH rw and ro modes. The absolute-path test above
// covers one vector; this locks the other against a future gbash bump (the
// whole no-escape guarantee is delegated to alpha-pinned gbash).
func TestBashbox_DotDotTraversalRefused(t *testing.T) {
	for _, ro := range []bool{false, true} {
		volRoot, _ := filepath.EvalSymlinks(t.TempDir())
		secretDir, _ := filepath.EvalSymlinks(t.TempDir()) // SIBLING of volRoot
		if err := os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("TOPSECRET"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := bashCtx(volRoot)
		if ro {
			ctx = roCtx(volRoot)
		}
		b := &Bashbox{Enabled: true}
		rel := filepath.Join("..", filepath.Base(secretDir), "secret.txt")
		res := runBashbox(t, b, ctx, "cat "+rel)
		if strings.Contains(res.Text, "TOPSECRET") {
			t.Fatalf("ro=%v: `..` traversal escaped the sandbox: %q", ro, res.Text)
		}
		if !res.IsError {
			t.Errorf("ro=%v: `..` traversal should fail; got %q", ro, res.Text)
		}
	}
}

// Escape vector #2 — a symlink INSIDE the volume pointing OUT to a host file
// is refused (gbash's SymlinkDeny + os.Root containment), in both modes.
func TestBashbox_SymlinkOutRefused(t *testing.T) {
	for _, ro := range []bool{false, true} {
		volRoot, _ := filepath.EvalSymlinks(t.TempDir())
		secretDir, _ := filepath.EvalSymlinks(t.TempDir())
		secret := filepath.Join(secretDir, "secret.txt")
		if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(volRoot, "escape")); err != nil {
			t.Fatal(err)
		}
		ctx := bashCtx(volRoot)
		if ro {
			ctx = roCtx(volRoot)
		}
		b := &Bashbox{Enabled: true}
		res := runBashbox(t, b, ctx, "cat escape")
		if strings.Contains(res.Text, "TOPSECRET") {
			t.Fatalf("ro=%v: symlink-out escaped the sandbox: %q", ro, res.Text)
		}
		if !res.IsError {
			t.Errorf("ro=%v: symlink-out should be refused; got %q", ro, res.Text)
		}
	}
}

// An EPHEMERAL read-only volume (the run-scoped path, resolved before the
// VolumePolicy) is honored as ro just like a static binding: writes succeed
// in-sandbox but never touch the host. Guards against a mis-wire of
// EphemeralVolumeRef.ReadOnly mounting it rw.
func TestBashbox_EphemeralRoVolume_DiscardsWrites(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := tools.NewEphemeralVolumeSet()
	set.Add("scratch", tools.EphemeralVolumeRef{Root: root, ReadOnly: true})
	ctx := tools.WithEphemeralVolumes(context.Background(), set)

	b := &Bashbox{Enabled: true}
	body, _ := json.Marshal(map[string]any{"command": "echo x > w.txt; cat w.txt", "volume": "scratch"})
	res, execErr := b.Execute(ctx, body)
	if execErr != nil {
		t.Fatal(execErr)
	}
	if res.IsError || !strings.Contains(res.Text, "x") {
		t.Fatalf("ephemeral ro write should succeed in-sandbox: err=%v out=%q", res.IsError, res.Text)
	}
	if _, statErr := os.Stat(filepath.Join(root, "w.txt")); !os.IsNotExist(statErr) {
		t.Errorf("ephemeral read-only volume leaked a write to the host: %v", statErr)
	}
}

// A wall-clock timeout is surfaced as an error with gbash's timeout line
// preserved. (gbash returns exit 124 + an "execution timed out" stderr on a
// deadline — NOT a Go error — so this exercises the exit-code path.)
func TestBashbox_TimeoutSurfacesAsError(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true}
	body, _ := json.Marshal(map[string]any{"command": "sleep 5", "timeout_seconds": 1})
	res, err := b.Execute(bashCtx(root), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("a timed-out command should be IsError; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "timed out") {
		t.Errorf("expected gbash timeout line in output; got %q", res.Text)
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
	if !strings.Contains(res.Text, "[output truncated at 100 bytes per stream]") {
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

// ── RFC AJ §13: operator host-command fallback ──────────────────────────────

func skipIfNoHostCmd(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("host command %q not available; skipping", name)
	}
}

// An operator-allowlisted host command (git — gbash has no git) runs on the
// real host instead of 127ing.
func TestBashbox_FallbackCommandRunsOnHost(t *testing.T) {
	skipIfNoHostCmd(t, "git")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"git"}}
	res := runBashbox(t, b, bashCtx(root), "git --version")
	if res.IsError {
		t.Fatalf("allowlisted git should run on host: %q", res.Text)
	}
	if !strings.Contains(res.Text, "git version") {
		t.Errorf("expected host git output; got %q", res.Text)
	}
}

// A non-allowlisted command stays sandboxed even when another (git) IS
// allowlisted: curl gets no host proxy, so it resolves only against gbash and
// 127s. gbash has no host-PATH fallback, so 127 is its baseline for any
// unregistered name — the DISCRIMINATING assertion is the "libcurl" check: if
// proxies were ever registered for non-allowlisted names, host curl would run
// and print "libcurl". (See also TestBashbox_FallbackProxyExactNameOnly.)
func TestBashbox_NonAllowlistedCommandStaysSandboxed(t *testing.T) {
	skipIfNoHostCmd(t, "curl")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"git"}}
	res := runBashbox(t, b, bashCtx(root), "curl --version")
	if !res.IsError {
		t.Fatalf("non-allowlisted curl must not escape the sandbox; got %q", res.Text)
	}
	if strings.Contains(res.Text, "libcurl") {
		t.Fatalf("curl reached the host (sandbox escape!): %q", res.Text)
	}
}

// A script mixing an allowlisted and a non-allowlisted command runs only the
// allowlisted one on the host; the other stays sandboxed (no smuggling).
func TestBashbox_SmugglingBlocked(t *testing.T) {
	skipIfNoHostCmd(t, "git")
	skipIfNoHostCmd(t, "curl")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"git"}}
	res := runBashbox(t, b, bashCtx(root), "git --version; curl --version")
	if !strings.Contains(res.Text, "git version") {
		t.Errorf("git (allowlisted) should have run on host: %q", res.Text)
	}
	if strings.Contains(res.Text, "libcurl") {
		t.Fatalf("curl (NOT allowlisted) reached the host inside a mixed script: %q", res.Text)
	}
}

// The allowlist is keyed to the command BASENAME, and the proxy always execs
// the operator's name via the host PATH (the model's path string is ignored —
// gbash resolves a path-form command by basename, and the proxy closure runs
// its captured name, not the model's spelling). So no spelling of a
// NON-allowlisted command — a sibling basename or a path-form — can reach a
// non-allowlisted host binary. (`git` is allowlisted here; curl is not.)
func TestBashbox_FallbackProxyExactNameOnly(t *testing.T) {
	skipIfNoHostCmd(t, "git")
	skipIfNoHostCmd(t, "curl")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"git"}}
	for _, cmd := range []string{
		"git-notathing --version", // sibling basename of an allowlisted name
		"curl --version",          // bare non-allowlisted command
		"/usr/bin/curl --version", // path-form of a non-allowlisted command
	} {
		res := runBashbox(t, b, bashCtx(root), cmd)
		if !res.IsError {
			t.Errorf("%q should be refused; got %q", cmd, res.Text)
		}
		if strings.Contains(res.Text, "libcurl") {
			t.Errorf("%q reached host curl (sandbox escape): %q", cmd, res.Text)
		}
	}
}

// A fallback command on a read-only volume refuses (a host process can't honor
// the in-RAM overlay) rather than writing to the real host behind a false ro.
func TestBashbox_FallbackRefusedOnRoVolume(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"git"}}
	res := runBashbox(t, b, roCtx(root), "git status")
	if !res.IsError {
		t.Fatalf("fallback on a ro volume must refuse; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "read-write volume") {
		t.Errorf("expected ro refusal message; got %q", res.Text)
	}
}

// The env-credential allowlist: an allowlisted var reaches the host command; a
// non-allowlisted secret does not; and the sandbox `env` (no fallback) sees
// neither, so the model can't read host credentials.
func TestBashbox_FallbackEnvAllowlist(t *testing.T) {
	skipIfNoHostCmd(t, "env")
	t.Setenv("LOOMCYCLE_FB_PUBLIC", "public-ok")
	t.Setenv("LOOMCYCLE_FB_SECRET", "secret-leak")
	// An AMBIENT secret-shaped var the operator never mentioned — the model's
	// real target. It must not reach the host child either.
	t.Setenv("AMBIENT_API_KEY", "sk-ambient-must-not-leak")
	root, _ := filepath.EvalSymlinks(t.TempDir())

	b := &Bashbox{Enabled: true, FallbackCommands: []string{"env"}, FallbackAllowedEnv: []string{"LOOMCYCLE_FB_PUBLIC"}}
	res := runBashbox(t, b, bashCtx(root), "env")
	if res.IsError {
		t.Fatalf("host env should run: %q", res.Text)
	}
	if !strings.Contains(res.Text, "LOOMCYCLE_FB_PUBLIC=public-ok") {
		t.Errorf("allowlisted var did not reach the host command: %q", res.Text)
	}
	if strings.Contains(res.Text, "secret-leak") {
		t.Fatalf("non-allowlisted secret leaked into the host command env: %q", res.Text)
	}
	if strings.Contains(res.Text, "sk-ambient-must-not-leak") {
		t.Fatalf("an ambient (never-allowlisted) secret leaked into the host command env: %q", res.Text)
	}

	// Without fallback, `env` is gbash's sandbox builtin — neither var is in the
	// sandbox env, so the model can't read host credentials via env.
	b2 := &Bashbox{Enabled: true}
	res2 := runBashbox(t, b2, bashCtx(root), "env")
	if strings.Contains(res2.Text, "public-ok") || strings.Contains(res2.Text, "secret-leak") {
		t.Errorf("host env leaked into the sandbox env: %q", res2.Text)
	}
}

// With no fallback configured, a gbash-unknown command (git) 127s as before.
func TestBashbox_FallbackDisabledByDefault(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true} // no FallbackCommands
	res := runBashbox(t, b, bashCtx(root), "git --version")
	if !res.IsError {
		t.Errorf("without fallback, git should 127; got %q", res.Text)
	}
}

// cwd translation: a fallback command runs in the host path for the script's
// current dir (cd sub → hostRoot/sub), proving it executes on the host (not the
// sandbox virtual "/") with the cwd mapped + containment-checked. Uses git
// (a registry-dispatched proxy) — `pwd`/`cd` are gbash shell builtins a proxy
// can't override. `git rev-parse --show-toplevel` prints the real host path.
func TestBashbox_FallbackHostCwdTranslated(t *testing.T) {
	skipIfNoHostCmd(t, "git")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"git"}}
	res := runBashbox(t, b, bashCtx(root), "mkdir -p sub && cd sub && git init -q && git rev-parse --show-toplevel")
	if res.IsError {
		t.Fatalf("host git should run: %q", res.Text)
	}
	want := filepath.Join(root, "sub")
	if !strings.Contains(res.Text, want) {
		t.Errorf("fallback git did not run in the translated host cwd %q; got %q", want, res.Text)
	}
}
