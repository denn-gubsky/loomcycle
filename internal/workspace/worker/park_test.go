package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/kernel"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// RFC GW P12 plan §3.3: park ends what runs in a durable workspace (its
// servers, its interpreters, its session) and keeps its directory, so a
// later run that pins it finds its files and nothing of the last run still
// running.

// forgetRecorder is an engine that records the sessions it was told to end.
type forgetRecorder struct {
	engine.Engine
	mu     sync.Mutex
	forgot []string
}

func (f *forgetRecorder) Forget(ctx context.Context, id string) error {
	f.mu.Lock()
	f.forgot = append(f.forgot, id)
	f.mu.Unlock()
	return f.Engine.Forget(ctx, id)
}

func park(w *worker, id string) answer {
	return w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: id, Op: workspace.OpPark}, nil)
}

func TestPark_EndsServersAndSessionAndKeepsTheFiles(t *testing.T) {
	requirePython(t)
	w, root := newTestWorker(t)
	rec := &forgetRecorder{Engine: w.engine}
	w.engine = rec
	writeFile(t, filepath.Join(root, "ws1", "keep.txt"), 5)
	port, pid := startChildServer(t, w, root)

	mustOK(t, park(w, "ws1"), "park")
	waitFor(t, "the server to stop listening", func() bool { return !listening(port) })
	waitFor(t, "the server's child to end", func() bool { return processGone(pid) })
	if got := w.servers.list("ws1"); len(got) != 0 {
		t.Errorf("servers after park = %d, want none", len(got))
	}
	rec.mu.Lock()
	forgot := append([]string(nil), rec.forgot...)
	rec.mu.Unlock()
	if len(forgot) != 1 || forgot[0] != "ws1" {
		t.Errorf("sessions ended = %v, want ws1's", forgot)
	}
	if b, err := os.ReadFile(filepath.Join(root, "ws1", "keep.txt")); err != nil || len(b) != 5 {
		t.Errorf("the file after park: %d bytes, %v", len(b), err)
	}
	// Nothing else of the worker's root went with it.
	if _, err := os.Stat(filepath.Join(root, "secret.txt")); err != nil {
		t.Errorf("a file beside the workspace: %v", err)
	}
	// A second park changes nothing and succeeds.
	mustOK(t, park(w, "ws1"), "a second park")
	if _, err := os.Stat(filepath.Join(root, "ws1", "keep.txt")); err != nil {
		t.Errorf("the file after a second park: %v", err)
	}
}

// An interpreter's variables are gone after park: the next cell runs in a
// new kernel and says so.
func TestPark_EndsTheInterpreters(t *testing.T) {
	requirePython(t)
	w, root := newTestWorker(t)
	w.kernels = kernel.NewManager(kernel.Options{Engine: w.engine, DriverDir: filepath.Join(root, driversDir), MarkDir: root})
	t.Cleanup(w.kernels.Close)
	real, err := filepath.EvalSymlinks(filepath.Join(root, "ws1"))
	if err != nil {
		t.Fatal(err)
	}
	cell := func(code string) workspace.RunCellAnswer {
		t.Helper()
		raw, _ := json.Marshal(workspace.RunCellArgs{Code: code, Kernel: "python", TimeoutMS: 30000, GraceMS: 2000})
		a := w.kernelOp(context.Background(), "ws1", real, workspace.OpRunCell, raw)
		if a.class != "" {
			t.Fatalf("cell %q: %s %s", code, a.class, a.detail)
		}
		var ans workspace.RunCellAnswer
		if err := json.Unmarshal(a.output, &ans); err != nil {
			t.Fatal(err)
		}
		return ans
	}
	cell("x = 41")
	if got := cell("x + 1"); got.Value == nil || *got.Value != "42" {
		t.Fatalf("before park: %+v", got)
	}
	mustOK(t, park(w, "ws1"), "park")
	got := cell("'x' in dir()")
	if got.Value == nil || *got.Value != "False" {
		t.Errorf("after park the variable is %+v, want gone", got)
	}
}

// park takes only a workspace on this worker; anything else is refused and
// touches nothing.
func TestPark_RefusesAWorkspaceNotHere(t *testing.T) {
	w, root := newTestWorker(t)
	for _, id := range []string{"ws9", "../ws1", ""} {
		if a := park(w, id); a.class == "" {
			t.Errorf("park %q succeeded", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ws1")); err != nil {
		t.Errorf("ws1 after refused parks: %v", err)
	}
}
