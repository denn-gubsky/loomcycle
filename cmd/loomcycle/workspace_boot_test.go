//go:build unix

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A runtime booted with workspace.mode remote serves the worker endpoint
// even with conversion in process, a `loomcycle worker --role workspace`
// holding a workers:attach token attaches to it, and one holding any other
// bearer is refused and exits.
func TestBoot_WorkspaceRemoteModeAttachesARealWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and boots the binary; skipped in -short")
	}
	b := newShutdownPauseBoot(t)
	endpoint := net.JoinHostPort("127.0.0.1", fmt.Sprint(freePort(t)))
	// Not a real credential: a value made for this boot.
	const workerToken = "test-workspace-worker-token-0123456789abcdef"
	cfg := shutdownPauseYAML(b.volume) +
		"principals:\n  wsworker:\n    tenant: acme\n    subject: worker-1\n    scopes: [workers:attach]\n    token_env: LOOMCYCLE_TEST_WORKSPACE_WORKER_TOKEN\n" +
		"worker_endpoint:\n  listen: " + endpoint + "\n" +
		"workspace:\n  mode: remote\n"
	if err := os.WriteFile(b.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	core := b.startOn(t, b.port, "LOOMCYCLE_TEST_WORKSPACE_WORKER_TOKEN="+workerToken)

	root := filepath.Join(b.dir, "workspaces")
	worker := func(token string) (*exec.Cmd, *lockedBuffer) {
		cmd := exec.Command(b.bin, "worker", "--role", "workspace", "--dispatcher", endpoint, "--id", "boot1")
		cmd.Env = []string{
			"HOME=" + filepath.Join(b.dir, "home"),
			"PATH=/usr/bin:/bin",
			"LOOMCYCLE_WORKER_TOKEN=" + token,
			"LOOMCYCLE_WORKSPACE_ROOT=" + root,
			"LOOMCYCLE_WORKSPACE_IDLE_STREAMS=2",
		}
		out := &lockedBuffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd, out
	}

	// A bearer the runtime does not know: refused for good.
	bad, badOut := worker("not-a-token")
	exited := make(chan error, 1)
	go func() { exited <- bad.Wait() }()
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("a refused worker exited %v, want status 2\n%s", err, badOut.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("a worker with a bad token kept running\n%s", badOut.String())
	}

	_, out := worker(workerToken)
	want := "worker_id=tok:cfg:wsworker/boot1"
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(core.out.String(), want); {
		if time.Now().After(deadline) {
			t.Fatalf("the worker did not attach\ncore:\n%s\nworker:\n%s", core.out.String(), out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(core.out.String(), "worker endpoint listening") {
		t.Errorf("the core did not report the endpoint\n%s", core.out.String())
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the worker made no root: %v", err)
	}
}
