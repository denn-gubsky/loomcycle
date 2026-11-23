package dispatch_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// A shared worker must isolate one tenant's commands from another's (D6 as
// amended): on the container engine it attaches and serves two tenants,
// each workspace's commands run in a session of their own that holds
// neither the worker's token nor the other tenant's files. A shared worker
// on the host engine is still refused (TestAttach_ASharedHostWorkerIsRefused).
func TestAttach_ASharedContainerWorkerServesTenantsInSessionsOfTheirOwn(t *testing.T) {
	cli, image := workspacetest.RequireDocker(t)
	// The token is in the worker's options and, as a deployment passes it,
	// in its environment.
	t.Setenv("LOOMCYCLE_WORKER_TOKEN", tokShared)
	d, target := core(t, newMemStore(), dispatch.Options{})
	root, done := realWorker(t, target, tokShared, "s1", worker.Options{IdleStreams: 2, Engine: workspace.EngineContainer,
		Container: engine.ContainerOptions{CLI: cli, Image: image, MemoryMB: 256, Pids: 128, TmpMB: 16}})
	// Starting a container engine probes the image first; a loaded daemon
	// can take a while.
	deadline := time.Now().Add(5 * time.Minute)
	for !attached(d, sharedID("s1")) {
		select {
		case err := <-done:
			t.Fatalf("the shared container worker stopped: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the shared container worker did not attach")
		}
		time.Sleep(50 * time.Millisecond)
	}

	ctx := context.Background()
	for _, tw := range [][2]string{{"acme", "ws-acme"}, {"other", "ws-other"}} {
		if _, err := d.Do(ctx, sharedID("s1"), dispatch.Request{TenantID: tw[0], WorkspaceID: tw[1], Op: workspace.OpOpen}); err != nil {
			t.Fatalf("open %s for %s: %v", tw[1], tw[0], err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "ws-other", "secret.txt"), []byte("the other tenant's secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := execIn(t, d, "acme", "ws-acme",
		`id -u; env; for f in /proc/[0-9]*/environ /proc/[0-9]*/cmdline; do cat "$f"; echo; done; `+
			`cat `+filepath.Join(root, "ws-other", "secret.txt")+` ../ws-other/secret.txt; find / -name secret.txt; ls -a /workspace`)
	if strings.Contains(out, tokShared) {
		t.Errorf("the worker's token is readable in a session:\n%s", out)
	}
	// The content, not the name: the command's own line names the file.
	if strings.Contains(out, "the other tenant's secret") {
		t.Errorf("another tenant's workspace is readable from a session:\n%s", out)
	}
	if strings.HasPrefix(out, "0\n") {
		t.Errorf("the session runs as root:\n%s", out)
	}
	if got := execIn(t, d, "other", "ws-other", "cat /workspace/secret.txt"); got != "the other tenant's secret" {
		t.Errorf("the other tenant's own session reads %q", got)
	}
}

func attached(d *dispatch.Dispatcher, id string) bool {
	for _, w := range d.Workers() {
		if w.ID == id {
			return true
		}
	}
	return false
}

// execIn runs command in a workspace through the dispatcher and returns its
// output; errors in the command are dropped, since what it could read is
// what the test looks at.
func execIn(t *testing.T, d *dispatch.Dispatcher, tenant, ws, command string) string {
	t.Helper()
	args, _ := json.Marshal(workspace.ExecArgs{Command: "(" + command + ") 2>/dev/null", TimeoutMS: 120_000})
	res, err := d.Do(context.Background(), sharedID("s1"), dispatch.Request{TenantID: tenant, WorkspaceID: ws, Op: workspace.OpExec, Args: args})
	if err != nil {
		t.Fatalf("exec in %s: %v", ws, err)
	}
	var a workspace.ExecAnswer
	if err := json.Unmarshal(res.Output, &a); err != nil {
		t.Fatalf("exec answer %q: %v", res.Output, err)
	}
	return a.Output
}
