//go:build linux

package procgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The main goroutine keeps the main thread, so every other goroutine runs on
// a thread Go can end. Go never ends the main thread: a goroutine that exits
// locked to it wedges it instead, and a child started there would survive
// whatever the code under test did.
func init() { runtime.LockOSThread() }

// TestMain doubles as the parent process for the parent-death test: run with
// PROCGROUP_TEST_PARENT=1, it starts a long-lived child the way the MCP
// client does, prints the child's pid, and waits to be killed.
func TestMain(m *testing.M) {
	if os.Getenv("PROCGROUP_TEST_PARENT") == "1" {
		child := exec.Command("sleep", "60")
		if err := StartInOwnGroup(child); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(child.Process.Pid)
		select {}
	}
	os.Exit(m.Run())
}

// running reports whether pid is a live process. A killed child stays a
// zombie until its parent reaps it, and kill(pid, 0) still succeeds on a
// zombie, so the state is read from /proc instead.
func running(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state is the field after the parenthesised command name.
	rest := string(b)[strings.LastIndexByte(string(b), ')')+1:]
	f := strings.Fields(rest)
	return len(f) > 0 && f[0] != "Z" && f[0] != "X"
}

// stops polls until pid is no longer a running process.
func stops(pid int, within time.Duration) bool {
	for deadline := time.Now().Add(within); running(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// A long-lived child dies when loomcycle dies, even when loomcycle is
// SIGKILLed and runs no cleanup (RFC EU D1).
func TestStartInOwnGroup_ChildDiesWithItsParent(t *testing.T) {
	parent := exec.Command(os.Args[0], "-test.run=^$")
	parent.Env = append(os.Environ(), "PROCGROUP_TEST_PARENT=1")
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = parent.Process.Kill()
		t.Fatalf("parent printed nothing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		_ = parent.Process.Kill()
		t.Fatalf("parent printed %q", line)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if !running(pid) {
		t.Fatalf("child %d is not running while its parent lives", pid)
	}

	_ = parent.Process.Kill()
	_ = parent.Wait()
	if !stops(pid, 3*time.Second) {
		t.Errorf("child %d outlived its SIGKILLed parent", pid)
	}
}

// Pdeathsig fires when the thread that started the child exits, not the
// process. A goroutine that locks its thread and returns without unlocking
// ends that thread. Here the caller does exactly that after starting the
// child; a child started from the caller's thread would be killed although
// its parent process is healthy. StartInOwnGroup starts every child from one
// thread that lives as long as the process, so the child survives.
func TestStartInOwnGroup_ChildSurvivesTheCallersThreadExiting(t *testing.T) {
	child := exec.Command("sleep", "60")
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: this goroutine's thread ends when it returns
		if syscall.Gettid() == os.Getpid() {
			started <- errors.New("the caller ran on the main thread, which never ends")
			return
		}
		started <- StartInOwnGroup(child)
	}()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = KillGroup(child); _ = child.Wait() })
	time.Sleep(300 * time.Millisecond) // let the caller's thread exit
	if !running(child.Process.Pid) {
		t.Errorf("child %d was killed while its parent process lives: Pdeathsig fired when the caller's thread exited", child.Process.Pid)
	}
}
