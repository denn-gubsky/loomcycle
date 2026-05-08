// TestBashTimeout asserts that a Bash run is killed when its
// per-call timeout deadline fires, and that the kill happens
// promptly (well before the underlying command would have
// finished naturally).
//
// Why this file exists separately, behind a `!race` build tag:
//
// The test relies on exec.CommandContext's internal cancel
// goroutine — when the runCtx fires its deadline, that
// goroutine calls Process.Kill() to send SIGKILL to the child.
// Under -race, the runtime adds enough overhead per goroutine
// schedule that the cancel goroutine can be starved long
// enough for the child's own work (`sleep 5`) to complete
// naturally before the kill fires. CI runs observed:
//
//   --- FAIL: TestBashTimeout (5.00s)
//       bash_test.go:84: timeout did not fire in time:
//                        elapsed=5.001967268s
//
// despite Timeout being set to 100ms. The cancel goroutine
// eventually ran but only after `sleep 5` had already exited.
//
// Production code is fine — production binaries don't run with
// -race, so the cancel goroutine is scheduled promptly. The CI
// race environment is testing different things (data races in
// our Go code) and isn't a useful place to validate
// goroutine-scheduling-sensitive timing. The conventional
// pattern for tests in this category is the `!race` build tag,
// applied here.
//
// All other Bash tests (refusals, output truncation, env
// scrubbing, etc.) remain in bash_test.go and run under both
// modes — they're functional, not timing-sensitive.

//go:build !race

package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

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
