//go:build unix

package kernel

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestKernel_ACellThatIgnoresTheInterruptIsKilledAndTheNextCellSaysRestarted(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	r := f.run(t, Python, "import os\nkept = 7\nos.getpid()", 10*time.Second)
	pid, err := strconv.Atoi(value(r))
	if err != nil {
		t.Fatalf("the kernel's pid: %s", value(r))
	}
	// A cell that ignores SIGINT stands in for a long call in native code,
	// which never sees the interrupt either.
	start := time.Now()
	r = f.run(t, Python, "import signal\nsignal.signal(signal.SIGINT, signal.SIG_IGN)\nwhile True: pass", 300*time.Millisecond)
	if !r.TimedOut || !r.KernelKilled || !strings.Contains(r.Error, "killed") {
		t.Fatalf("stuck cell: timed out %v killed %v error %q", r.TimedOut, r.KernelKilled, r.Error)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the kill took %s", d)
	}
	// The stuck driver is gone, not left spinning. The engine reaps it, so
	// once dead it is not a zombie that signal 0 still finds.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the killed kernel (pid %d) is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r = f.run(t, Python, "'kept' in globals()", 10*time.Second)
	if !r.KernelRestarted || value(r) != "False" || r.ExecutionCount != 1 {
		t.Fatalf("next cell: restarted %v value %s count %d", r.KernelRestarted, value(r), r.ExecutionCount)
	}
	r = f.run(t, Python, "1", 10*time.Second)
	if r.KernelRestarted {
		t.Errorf("the cell after the restarted one says restarted again")
	}
}
