//go:build unix

package main

import (
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
// holding a workers:attach token attaches to it, and one holding a bearer
// the runtime cannot verify is never attached (it keeps retrying: an
// unresolved token is not a final refusal).
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
	worker := func(token, id string) (*exec.Cmd, *lockedBuffer) {
		cmd := exec.Command(b.bin, "worker", "--role", "workspace", "--dispatcher", endpoint, "--id", id)
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

	// A bearer the runtime does not know: never attached, still retrying.
	bad, badOut := worker("not-a-token", "bad1")
	exited := make(chan struct{})
	go func() { _ = bad.Wait(); close(exited) }()

	_, out := worker(workerToken, "boot1")
	want := "worker_id=tenant:acme/boot1"
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
	if strings.Contains(core.out.String(), "tenant:acme/bad1") {
		t.Errorf("a worker with an unknown bearer attached\n%s", core.out.String())
	}
	select {
	case <-exited:
		t.Errorf("a worker with an unknown bearer stopped; it should retry\n%s", badOut.String())
	default:
	}
}
