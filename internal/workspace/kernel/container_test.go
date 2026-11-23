package kernel_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/kernel"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// kernelImageEnv names the session image the container kernel tests run:
// one built from Dockerfile.workspace, which carries the drivers at
// kernel.ImageDriversDir and has python3 and node. CI's workspace-container
// job builds it and sets this, with LOOMCYCLE_REQUIRE_DOCKER, so a skip
// there is a failure.
const kernelImageEnv = "LOOMCYCLE_TEST_KERNEL_IMAGE"

type containerKernel struct {
	m   *kernel.Manager
	dir string
}

func newContainerKernel(t *testing.T) *containerKernel {
	t.Helper()
	cli, _ := workspacetest.RequireDocker(t)
	image := os.Getenv(kernelImageEnv)
	if image == "" {
		if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
			t.Fatalf("%s is not set: build Dockerfile.workspace and name it", kernelImageEnv)
		}
		t.Skipf("%s is not set", kernelImageEnv)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := engine.NewContainer(engine.ContainerOptions{
		CLI: cli, Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), WorkerKey: engine.WorkerKey(t.Name(), root),
		MemoryMB: 512, Pids: 256, TmpMB: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	dir := filepath.Join(root, "ws1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := kernel.NewManager(kernel.Options{Engine: c, MarkDir: root, StartWait: 2 * time.Minute,
		Drivers: &kernel.Drivers{Python: kernel.ImageDriversDir + "/python_driver.py", Node: kernel.ImageDriversDir + "/node_driver.js"}})
	t.Cleanup(m.Close)
	return &containerKernel{m: m, dir: dir}
}

func (k *containerKernel) run(t *testing.T, kind, code string, timeout time.Duration) kernel.Result {
	t.Helper()
	res, err := k.m.Run(context.Background(), "ws1", kernel.Request{Kind: kind, Code: code, Dir: k.dir, Timeout: timeout, Grace: 2 * time.Second})
	if err != nil {
		t.Fatalf("run %q: %v", code, err)
	}
	return res
}

func val(r kernel.Result) string {
	if r.Value == nil {
		return "<none>"
	}
	return *r.Value
}

// In a session, the kernels run from the drivers the image carries, keep
// their variables, write into the mounted workspace, and are interrupted
// and killed through the engine.
func TestContainerKernel_CellsRunInTheSessionAndKeepTheirVariables(t *testing.T) {
	k := newContainerKernel(t)
	r := k.run(t, kernel.Python, "import os\nx = 41\nopen('made.txt', 'w').write(os.getcwd())\nprint('hi')", time.Minute)
	if r.Output != "hi\n" || r.Error != "" {
		t.Fatalf("first python cell: output %q error %q", r.Output, r.Error)
	}
	if b, err := os.ReadFile(filepath.Join(k.dir, "made.txt")); err != nil || string(b) != engine.WorkspaceMount {
		t.Fatalf("the cell's file on the worker's disk = %q, %v; want the cwd %s", b, err, engine.WorkspaceMount)
	}
	if r := k.run(t, kernel.Python, "x + 1", time.Minute); val(r) != "42" {
		t.Fatalf("second python cell: %s %q", val(r), r.Error)
	}
	k.run(t, kernel.Node, "let a = 40; const b = await Promise.resolve(2)", time.Minute)
	if r := k.run(t, kernel.Node, "a + b", time.Minute); val(r) != "42" {
		t.Fatalf("node: %s %q", val(r), r.Error)
	}
}

func TestContainerKernel_AnInterruptKeepsStateAndAStuckCellIsKilled(t *testing.T) {
	k := newContainerKernel(t)
	k.run(t, kernel.Python, "kept = 7", time.Minute)
	r := k.run(t, kernel.Python, "while True: pass", time.Second)
	if !r.TimedOut || !r.Interrupted || r.KernelKilled {
		t.Fatalf("loop: %+v", r)
	}
	if r := k.run(t, kernel.Python, "kept", time.Minute); val(r) != "7" {
		t.Fatalf("after the interrupt: %s %q", val(r), r.Error)
	}
	r = k.run(t, kernel.Python, "import signal\nsignal.signal(signal.SIGINT, signal.SIG_IGN)\nwhile True: pass", time.Second)
	if !r.KernelKilled || !strings.Contains(r.Error, "killed") {
		t.Fatalf("stuck cell: %+v", r)
	}
	r = k.run(t, kernel.Python, "'kept' in globals()", time.Minute)
	if !r.KernelRestarted || val(r) != "False" {
		t.Fatalf("after the kill: %+v", r)
	}
}
