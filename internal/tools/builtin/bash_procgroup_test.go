//go:build unix

package builtin

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A host command's bound is checked against a generous ceiling: the unfixed
// code blocks for the full `sleep 60`, so 10 s still discriminates while
// leaving room for -race scheduling delays (see bash_timeout_test.go).
const procGroupBound = 10 * time.Second

var bgPIDRe = regexp.MustCompile(`bg=(\d+)`)

// bgPID extracts the `bg=<pid>` marker a test command prints and registers a
// cleanup that kills the process, so a failing run never leaves a `sleep 60`
// behind.
func bgPID(t *testing.T, out string) int {
	t.Helper()
	m := bgPIDRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no bg=<pid> marker in output %q", out)
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// processGone polls kill(pid, 0) until it reports ESRCH. A SIGKILLed child is
// reparented to init and reaped shortly after, so a brief zombie is allowed.
func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A background child that inherits stdout must not hold the call past its
// timeout: the timeout kills the whole process group, so the pipe closes and
// the backgrounded `sleep` is gone too.
func TestBash_BackgroundChildHoldingStdoutDoesNotHangPastTimeout(t *testing.T) {
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true, Timeout: time.Second}
	body, _ := json.Marshal(map[string]any{"command": "sleep 60 & echo bg=$!; echo started; sleep 60"})

	start := time.Now()
	res, err := b.Execute(bashCtx(cwd), body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	pid := bgPID(t, res.Text)
	if elapsed > procGroupBound {
		t.Fatalf("call returned after %v, want < %v (a background child held stdout)", elapsed, procGroupBound)
	}
	if !res.IsError || !strings.Contains(res.Text, "[killed:") {
		t.Errorf("want a timeout error, got %q", res.Text)
	}
	if !processGone(pid, 2*time.Second) {
		t.Errorf("background child %d survived the timeout; the process group was not killed", pid)
	}
}

// A command that exits normally while a background child keeps stdout open
// returns promptly with its output, and the background child keeps running:
// the group is killed only on timeout or cancel.
func TestBash_NormalExitWithBackgroundChildReturns(t *testing.T) {
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true}
	body, _ := json.Marshal(map[string]any{"command": "sleep 60 & echo bg=$!; echo ok"})

	start := time.Now()
	res, err := b.Execute(bashCtx(cwd), body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	pid := bgPID(t, res.Text)
	if elapsed > procGroupBound {
		t.Fatalf("call returned after %v, want < %v (a background child held stdout)", elapsed, procGroupBound)
	}
	if res.IsError || !strings.Contains(res.Text, "ok") {
		t.Errorf("want a successful result with ok, got IsError=%v %q", res.IsError, res.Text)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("background child %d was killed on a normal exit: %v", pid, err)
	}
}

// The Bashbox host-command fallback has the same shape as Bash: a host child
// whose background descendant holds the output must not outlive the timeout.
func TestBashbox_FallbackHangBounded(t *testing.T) {
	skipIfNoHostCmd(t, "sh")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"sh"}}
	body, _ := json.Marshal(map[string]any{
		"command":         `sh -c 'sleep 60 & echo bg=$!; sleep 60'`,
		"timeout_seconds": 1,
	})

	start := time.Now()
	res, err := b.Execute(bashCtx(root), body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	pid := bgPID(t, res.Text)
	if elapsed > procGroupBound {
		t.Fatalf("call returned after %v, want < %v (a background child held the output)", elapsed, procGroupBound)
	}
	if !res.IsError {
		t.Errorf("a timed-out fallback command should be IsError; got %q", res.Text)
	}
	if !processGone(pid, 2*time.Second) {
		t.Errorf("background child %d survived the timeout; the process group was not killed", pid)
	}
}

// A command that exits 0 while a background child still holds stdout, and
// whose timeout then fires inside the output-drain window, succeeded: the
// context was not consulted after the exit, so nothing was killed. It must
// not be reported as a timeout kill.
func TestBash_ExitZeroThenTimeoutDuringOutputDrainIsSuccess(t *testing.T) {
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bash{Enabled: true, Timeout: time.Second}
	body, _ := json.Marshal(map[string]any{"command": "sleep 60 & echo bg=$!; sleep 0.3"})

	res, err := b.Execute(bashCtx(cwd), body)
	if err != nil {
		t.Fatal(err)
	}
	pid := bgPID(t, res.Text)
	if res.IsError || strings.Contains(res.Text, "[killed:") {
		t.Errorf("an exit-0 command was reported as killed: IsError=%v %q", res.IsError, res.Text)
	}
	if !strings.Contains(res.Text, "[output closed") {
		t.Errorf("want the output-closed note, got %q", res.Text)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("background child %d was killed although the command exited on its own: %v", pid, err)
	}
}

// The fallback tells the model why output stopped, as Bash does, when a
// background process kept it open after the command exited.
func TestBashbox_FallbackNormalExitWithBackgroundChildNotesClosedOutput(t *testing.T) {
	skipIfNoHostCmd(t, "sh")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &Bashbox{Enabled: true, FallbackCommands: []string{"sh"}}
	res := runBashbox(t, b, bashCtx(root), `sh -c 'sleep 60 & echo bg=$!'`)
	bgPID(t, res.Text)
	if res.IsError {
		t.Errorf("an exit-0 fallback command should succeed; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "[output closed") {
		t.Errorf("want the output-closed note, got %q", res.Text)
	}
}
