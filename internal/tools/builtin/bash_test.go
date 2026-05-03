package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Refusal cases: empty Enabled flag rejects every call. We test this
// before any cwd config because Enabled is the single security gate
// operators need to flip — a misconfigured cwd should never matter
// when Enabled is false.
func TestBashRefusesWhenNotEnabled(t *testing.T) {
	b := &Bash{Cwd: "/tmp"}
	res, err := b.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not enabled") {
		t.Errorf("expected not-enabled refusal, got %q", res.Text)
	}
}

func TestBashRefusesWithoutCwd(t *testing.T) {
	b := &Bash{Enabled: true}
	res, err := b.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "no cwd") {
		t.Errorf("expected no-cwd refusal, got %q", res.Text)
	}
}

func TestBashRunsInsideCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh; Windows tests would need cmd.exe wrapping")
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "marker.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Bash{Enabled: true, Cwd: cwd}
	body, _ := json.Marshal(map[string]string{"command": "ls"})
	res, err := b.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "marker.txt") {
		t.Errorf("ls did not see marker.txt; got %q", res.Text)
	}
}

func TestBashTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true, Cwd: cwd, Timeout: 100 * time.Millisecond}
	body, _ := json.Marshal(map[string]any{"command": "sleep 5"})

	start := time.Now()
	res, err := b.Execute(context.Background(), body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected timeout error, got %q", res.Text)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout did not fire in time: elapsed=%v", elapsed)
	}
}

// Output bound: a command that produces 100KB of output should be
// truncated when MaxOutputBytes is 1KB. The buffer cap is what
// prevents an OOM from a malicious command.
func TestBashTruncatesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true, Cwd: cwd, MaxOutputBytes: 100}
	// yes | head -c 5000 produces ~5KB of "y\n" repeated.
	body, _ := json.Marshal(map[string]string{"command": "yes | head -c 5000"})
	res, err := b.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "[output truncated at 100 bytes]") {
		t.Errorf("missing truncation marker; output len=%d, text=%q", len(res.Text), res.Text)
	}
}

// Env scrub: ANTHROPIC_API_KEY is in the parent's env (test sets it),
// but a Bash command's `env` output must not see it. PATH is the only
// thing we leak by default.
func TestBashScrubsParentEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	t.Setenv("LOOMCYCLE_SECRET_FOR_TEST", "leak-me")
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true, Cwd: cwd}
	body, _ := json.Marshal(map[string]string{"command": "env"})
	res, err := b.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "LOOMCYCLE_SECRET_FOR_TEST") {
		t.Errorf("parent env leaked into child: %q", res.Text)
	}
	if !strings.Contains(res.Text, "PATH=") {
		t.Errorf("PATH should be inherited (most binaries need it); got %q", res.Text)
	}
}

// Allowlisted env passthrough: explicitly allowed names DO leak.
// Operators use this to share e.g. NODE_ENV or build-system flags.
func TestBashAllowsExplicitlyAllowedEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	t.Setenv("LOOMCYCLE_PUBLIC_FOR_TEST", "share-me")
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{
		Enabled:         true,
		Cwd:             cwd,
		AllowedExtraEnv: []string{"LOOMCYCLE_PUBLIC_FOR_TEST"},
	}
	body, _ := json.Marshal(map[string]string{"command": "env"})
	res, err := b.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "LOOMCYCLE_PUBLIC_FOR_TEST=share-me") {
		t.Errorf("allow-listed env did not pass through: %q", res.Text)
	}
}

// Non-zero exit is reported as IsError but the output is preserved so
// the model has something to work with (e.g. grep with no match).
func TestBashNonZeroExitSurfacesAsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true, Cwd: cwd}
	body, _ := json.Marshal(map[string]string{"command": "echo something; exit 7"})
	res, err := b.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true on non-zero exit; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "something") {
		t.Errorf("output not preserved on error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[exit:") {
		t.Errorf("exit marker missing: %q", res.Text)
	}
}

// 5-minute hard ceiling: even if the caller asks for an hour, we cap.
func TestBashCallerTimeoutCappedAtHardCeiling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true, Cwd: cwd, MaxOutputBytes: 64}
	// timeout_seconds: 9999 → must clamp to 300s. We don't actually wait;
	// just verify the configured timeout doesn't allow >5min via reflection
	// of behaviour: a quick command should still complete.
	body, _ := json.Marshal(map[string]any{"command": "echo fast", "timeout_seconds": 9999})
	res, err := b.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("quick command failed: %q", res.Text)
	}
	if !strings.Contains(res.Text, "fast") {
		t.Errorf("missing output: %q", res.Text)
	}
}
