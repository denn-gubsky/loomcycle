//go:build unix

package mcp_test

import (
	"context"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/stdio"
)

// A stdio server launched through a wrapper (npx, uvx, a script) that never
// answers initialize: the handshake times out, the pool tears the half-built
// client down, and Get must return the handshake error. Before the server ran
// in its own process group, the teardown killed only the wrapper and then
// waited for the real server to release stdout, so the tool call that
// triggered the lazy start hung for as long as that server lived.
func TestPool_HandshakeFailureWithWrappedServerReturnsTheError(t *testing.T) {
	bgPID := regexp.MustCompile(`bg=(\d+)`)
	var (
		mu   sync.Mutex
		pids []int
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	build := func(_, _ string) (mcp.Caller, error) {
		return stdio.Spawn(stdio.Config{
			Command: "sh",
			Args:    []string{"-c", "sleep 60 & echo bg=$! >&2; cat >/dev/null"},
			OnStderr: func(line string) {
				if m := bgPID.FindStringSubmatch(line); m != nil {
					if pid, err := strconv.Atoi(m[1]); err == nil {
						mu.Lock()
						pids = append(pids, pid)
						mu.Unlock()
					}
				}
			},
		})
	}
	teardown := func(c mcp.Caller) {
		if cl, ok := c.(*stdio.Client); ok {
			_ = cl.Close()
		}
	}
	pool := mcp.NewPool(build, teardown, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	type result struct{ err error }
	got := make(chan result, 1)
	start := time.Now()
	go func() {
		_, _, err := pool.Get(ctx, "wrapped")
		got <- result{err}
	}()
	select {
	case r := <-got:
		if r.err == nil {
			t.Fatal("Get succeeded against a server that never answers initialize")
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("Get returned after %v; want the handshake error within the close bound", took)
		}
	case <-time.After(5 * time.Second):
		// Release the hung teardown so the test ends, then fail.
		mu.Lock()
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		mu.Unlock()
		<-got
		t.Fatalf("Get was still blocked after 5s: tearing down a wrapped server waited for the server it started")
	}
}
