//go:build unix

package stdio

import (
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// closeBound is the ceiling a test allows Close: the bounded wait plus room
// for -race scheduling. The unfixed Close waits for the grandchild's `sleep
// 60`, so 5 s still tells the two apart.
const closeBound = 5 * time.Second

var bgPIDLine = regexp.MustCompile(`bg=(\d+)`)

// spawnWrapper starts script under sh, the way npx or uvx starts the real
// server as its own child, and returns the client and the pid of the
// background process the script reports on stderr as bg=<pid>. That process
// inherits the server's stdout. A cleanup kills it, so a failing run never
// leaves a `sleep 60` behind.
func spawnWrapper(t *testing.T, script string) (*Client, int) {
	t.Helper()
	pids := make(chan int, 1)
	var once sync.Once
	c, err := Spawn(Config{
		Command: "sh",
		Args:    []string{"-c", script},
		OnStderr: func(line string) {
			if m := bgPIDLine.FindStringSubmatch(line); m != nil {
				if pid, err := strconv.Atoi(m[1]); err == nil {
					once.Do(func() { pids <- pid })
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case pid := <-pids:
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		return c, pid
	case <-time.After(5 * time.Second):
		_ = c.Close()
		t.Fatal("the wrapper never reported its background pid")
		return nil, 0
	}
}

// closeWithin runs Close and reports how long it took. Past the bound it
// kills grandchild so the unfixed Close returns and the test can fail
// instead of hanging.
func closeWithin(t *testing.T, c *Client, grandchild int) (time.Duration, bool) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
		return time.Since(start), true
	case <-time.After(closeBound):
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		<-done
		return time.Since(start), false
	}
}

// gone polls kill(pid, 0) until it reports ESRCH. A killed process is
// reparented to init and reaped shortly after, so a brief zombie is allowed.
func gone(pid int, within time.Duration) bool {
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

// A server started through a wrapper leaves the real server holding stdout
// when the wrapper is killed. Close must still return, and the real server
// must be gone: Close kills the server's whole process group.
func TestClose_GrandchildHoldingStdoutReturnsPromptly(t *testing.T) {
	c, pid := spawnWrapper(t, "sleep 60 & echo bg=$! >&2; cat >/dev/null")
	took, ok := closeWithin(t, c, pid)
	if !ok {
		t.Fatalf("Close blocked past %v while a grandchild held stdout (returned after %v only once it was killed)", closeBound, took)
	}
	if !gone(pid, 2*time.Second) {
		t.Errorf("grandchild %d survived Close; the server's process group was not killed", pid)
	}
}

// A descendant that calls setsid() leaves the process group, so the group
// kill misses it and it keeps stdout open. Close must not wait for it: past
// its bound it closes its end of the pipes. The descendant may survive.
func TestClose_SetsidGrandchildDoesNotBlockClose(t *testing.T) {
	detached := "setsid sleep 60"
	if _, err := exec.LookPath("setsid"); err != nil {
		// macOS has no setsid command; perl's POSIX::setsid is the same call.
		if _, err := exec.LookPath("perl"); err != nil {
			t.Skip("neither setsid nor perl on this host")
		}
		detached = `perl -MPOSIX -e 'POSIX::setsid(); exec "sleep", "60"'`
	}
	c, pid := spawnWrapper(t, detached+" & echo bg=$! >&2; cat >/dev/null")
	// The pid is reported before the descendant has called setsid; wait
	// until it leads its own session, or the group kill still reaches it and
	// the bounded wait is never exercised.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if sid, err := unix.Getsid(pid); err == nil && sid == pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d never left the process group", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	took, ok := closeWithin(t, c, pid)
	if !ok {
		t.Fatalf("Close blocked past %v on a descendant that left the process group (returned after %v only once it was killed)", closeBound, took)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the detached descendant did not survive the group kill (%v), so the bounded wait was not exercised", err)
	}
}
