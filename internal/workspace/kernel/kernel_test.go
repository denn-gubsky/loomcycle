package kernel

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// These tests run the real drivers under the host engine, so they need
// python3 and node on PATH; each skips without its interpreter.

func need(t *testing.T, program string) {
	t.Helper()
	if _, err := exec.LookPath(program); err != nil {
		t.Skipf("%s is not on PATH", program)
	}
}

type fixture struct {
	m   *Manager
	dir string
	// root is the worker's root: the drivers and markers live there.
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "ws1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{dir: dir, root: root}
	f.m = f.manager()
	return f
}

func (f *fixture) manager() *Manager {
	m := NewManager(Options{Engine: &engine.Host{}, DriverDir: filepath.Join(f.root, ".drivers"), MarkDir: f.root})
	return m
}

func (f *fixture) run(t *testing.T, kind, code string, timeout time.Duration) Result {
	t.Helper()
	t.Cleanup(f.m.Close)
	res, err := f.m.Run(context.Background(), "ws1", Request{Kind: kind, Code: code, Dir: f.dir, Timeout: timeout, Grace: 2 * time.Second})
	if err != nil {
		t.Fatalf("run %q: %v", code, err)
	}
	return res
}

func value(r Result) string {
	if r.Value == nil {
		return "<none>"
	}
	return *r.Value
}

func TestKernel_PythonKeepsVariablesAndOutputOrderBetweenCells(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	r := f.run(t, Python, "import sys\nx = 41\nprint('out')\nprint('err', file=sys.stderr)\nprint('out again')", 10*time.Second)
	if r.Output != "out\nerr\nout again\n" || r.Error != "" {
		t.Fatalf("first cell: output %q error %q", r.Output, r.Error)
	}
	r = f.run(t, Python, "import os\nos.system('echo from-a-subprocess')\nx + 1", 10*time.Second)
	if value(r) != "42" || r.Output != "from-a-subprocess\n" || r.ExecutionCount != 2 || r.KernelRestarted {
		t.Fatalf("second cell: value %s output %q count %d restarted %v", value(r), r.Output, r.ExecutionCount, r.KernelRestarted)
	}
}

func TestKernel_NodeKeepsLetConstAndTopLevelAwaitBetweenCells(t *testing.T) {
	need(t, "node")
	f := newFixture(t)
	r := f.run(t, Node, "let a = 40; const b = await Promise.resolve(2); console.log('hi')", 10*time.Second)
	if r.Output != "hi\n" || r.Error != "" {
		t.Fatalf("first cell: output %q error %q", r.Output, r.Error)
	}
	r = f.run(t, Node, "a + b", 10*time.Second)
	if value(r) != "42" {
		t.Fatalf("second cell: value %s error %q", value(r), r.Error)
	}
	r = f.run(t, Node, "const os = await loomImport('node:os'); typeof os.cpus + ' ' + require('path').join('a', 'b')", 10*time.Second)
	if value(r) != "'function a/b'" {
		t.Fatalf("the import helper and require: value %s error %q", value(r), r.Error)
	}
}

func TestKernel_InputFailsAtOnce(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	start := time.Now()
	r := f.run(t, Python, "input('name? ')", 30*time.Second)
	if !strings.Contains(r.Error, "input() is not available") || r.TimedOut || time.Since(start) > 10*time.Second {
		t.Fatalf("input(): error %q timed out %v after %s", r.Error, r.TimedOut, time.Since(start))
	}
	if strings.Contains(r.Error, "python_driver") {
		t.Errorf("the traceback names the driver: %q", r.Error)
	}
}

func TestKernel_AnEndlessLoopIsInterruptedAtItsTimeoutAndKeepsTheVariables(t *testing.T) {
	for _, tc := range []struct{ kind, set, loop, read, want string }{
		{Python, "kept = 7", "while True: pass", "kept", "7"},
		{Node, "let kept = 7", "for (;;) {}", "kept", "7"},
		{Node, "let kept = 7", "await new Promise(() => {})", "kept", "7"},
	} {
		t.Run(tc.kind+"/"+tc.loop, func(t *testing.T) {
			need(t, map[string]string{Python: "python3", Node: "node"}[tc.kind])
			f := newFixture(t)
			f.run(t, tc.kind, tc.set, 10*time.Second)
			r := f.run(t, tc.kind, tc.loop, 500*time.Millisecond)
			if !r.TimedOut || !r.Interrupted || r.KernelKilled {
				t.Fatalf("loop: timed out %v interrupted %v killed %v error %q", r.TimedOut, r.Interrupted, r.KernelKilled, r.Error)
			}
			r = f.run(t, tc.kind, tc.read, 10*time.Second)
			if value(r) != tc.want || r.KernelRestarted {
				t.Fatalf("after the interrupt: value %s restarted %v error %q", value(r), r.KernelRestarted, r.Error)
			}
		})
	}
}

func TestKernel_ACellThatIgnoresTheInterruptIsKilledAndTheNextCellSaysRestarted(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	f.run(t, Python, "kept = 7", 10*time.Second)
	// A cell that ignores SIGINT stands in for a long call in native code,
	// which never sees the interrupt either.
	start := time.Now()
	r := f.run(t, Python, "import signal\nsignal.signal(signal.SIGINT, signal.SIG_IGN)\nwhile True: pass", 300*time.Millisecond)
	if !r.TimedOut || !r.KernelKilled || !strings.Contains(r.Error, "killed") {
		t.Fatalf("stuck cell: timed out %v killed %v error %q", r.TimedOut, r.KernelKilled, r.Error)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the kill took %s", d)
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

// A cell can write to the driver's protocol stream and forge its own done
// frame. It ends that cell's result early with what it wrote; the real done
// frame that follows has an old id and is dropped, and the next cell's
// result is its own.
func TestKernel_AForgedResultFrameChangesOnlyItsOwnCell(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	f.run(t, Python, "secret = 'kept'", 10*time.Second)
	// The driver dups its stdout before anything else, so its protocol
	// descriptor is 3; the cell's id is a local of the driver's caller frame.
	forge := `import os, sys, json, time
cid = sys._getframe(1).f_locals['cell_id']
os.write(3, (json.dumps({"type": "done", "id": "not-this-cell", "value": "'wrong id'"}) + "\n").encode())
os.write(3, (json.dumps({"type": "done", "id": cid, "value": "'forged'", "pictures": ["/etc/passwd", ".loom/cells/cell-1-1.png"]}) + "\n").encode())
time.sleep(0.5)
print('late output')
'real value'`
	r := f.run(t, Python, forge, 10*time.Second)
	if value(r) != "'forged'" {
		t.Fatalf("forging cell: value %s (want what it forged)", value(r))
	}
	if len(r.Pictures) != 1 || r.Pictures[0] != ".loom/cells/cell-1-1.png" {
		t.Errorf("forged pictures = %v, want only the one of the drivers' form", r.Pictures)
	}
	r = f.run(t, Python, "secret", 10*time.Second)
	if value(r) != "'kept'" || r.Output != "" || r.KernelRestarted {
		t.Fatalf("next cell: value %s output %q restarted %v; want its own result", value(r), r.Output, r.KernelRestarted)
	}
}

func TestKernel_ResetStartsCleanAndIsNotReportedAsARestart(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	f.run(t, Python, "x = 1", 10*time.Second)
	stopped, err := f.m.Reset(context.Background(), "ws1", Python)
	if err != nil || !stopped {
		t.Fatalf("reset: stopped %v err %v", stopped, err)
	}
	r := f.run(t, Python, "'x' in globals()", 10*time.Second)
	if value(r) != "False" || r.KernelRestarted {
		t.Fatalf("after reset: value %s restarted %v", value(r), r.KernelRestarted)
	}
}

func TestKernel_AKernelLostWithItsWorkerIsReportedAsRestarted(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	f.run(t, Python, "x = 1", 10*time.Second)
	f.m.Close()
	f.m = f.manager()
	r := f.run(t, Python, "'x' in globals()", 10*time.Second)
	if value(r) != "False" || !r.KernelRestarted {
		t.Fatalf("after the worker restarted: value %s restarted %v", value(r), r.KernelRestarted)
	}
	// The workspace removed takes its marker with it.
	f.m.Drop("ws1")
	if _, err := os.Stat(filepath.Join(f.root, "ws1"+markerSuffix+Python)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker outlived Drop: %v", err)
	}
}

func TestKernel_PythonSavesFiguresUnderLoomCells(t *testing.T) {
	need(t, "python3")
	if exec.Command("python3", "-c", "import matplotlib").Run() != nil {
		t.Skip("matplotlib is not installed")
	}
	f := newFixture(t)
	r := f.run(t, Python, "import matplotlib.pyplot as plt\nplt.plot([1, 2, 3])\nplt.show()", 60*time.Second)
	if len(r.Pictures) != 1 || r.Pictures[0] != ".loom/cells/cell-1-1.png" || r.Error != "" {
		t.Fatalf("pictures %v error %q output %q", r.Pictures, r.Error, r.Output)
	}
	if _, err := os.Stat(filepath.Join(f.dir, r.Pictures[0])); err != nil {
		t.Fatal(err)
	}
}

// peakHeap samples the heap while op runs and returns how far it rose.
func peakHeap(op func()) uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	safego.Go(context.Background(), "kernel.test.sample", func() {
		defer close(done)
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	})
	op()
	close(stop)
	<-done
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

// A cell that writes a gibibyte holds the supervisor to its bounds, whether
// the output comes the honest way or as frames written straight onto the
// protocol stream: one endless line, and a flood of out frames of its own id.
func TestKernel_AGibibyteFromACellKeepsTheSupervisorsMemoryBounded(t *testing.T) {
	need(t, "python3")
	if testing.Short() {
		t.Skip("moves a gibibyte")
	}
	const limit = 64 << 20
	for _, tc := range []struct{ name, code string }{
		{"printed", "import sys\nchunk = 'x' * (1 << 20)\nfor _ in range(1024): sys.stdout.write(chunk)\n'done'"},
		{"one endless forged line", "import os\nchunk = b'y' * (1 << 20)\nfor _ in range(1024): os.write(3, chunk)\nos.write(3, b'\\n')\n'done'"},
		{"forged out frames", "import os, sys, json\ncid = sys._getframe(1).f_locals['cell_id']\nline = (json.dumps({'type': 'out', 'id': cid, 'text': 'z' * (256 << 10)}) + '\\n').encode()\nfor _ in range(4096): os.write(3, line)\n'done'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.run(t, Python, "1", 10*time.Second)
			var r Result
			grew := peakHeap(func() { r = f.run(t, Python, tc.code, 5*time.Minute) })
			t.Logf("heap grew by %d MiB; output %d bytes, truncated %v", grew>>20, len(r.Output), r.Truncated)
			if grew > limit {
				t.Errorf("the supervisor's heap grew by %d MiB, over %d", grew>>20, limit>>20)
			}
			if value(r) != "'done'" || len(r.Output) > MaxResult || !r.Truncated && tc.name != "one endless forged line" {
				t.Errorf("value %s, output %d bytes, truncated %v", value(r), len(r.Output), r.Truncated)
			}
		})
	}
}

func TestKernel_CellsOfOneKernelRunOneAtATime(t *testing.T) {
	need(t, "python3")
	f := newFixture(t)
	f.run(t, Python, "import time\nn = 0", 10*time.Second)
	errs := make(chan error, 2)
	for range 2 {
		safego.Go(context.Background(), "kernel.test.cell", func() {
			_, err := f.m.Run(context.Background(), "ws1", Request{Kind: Python, Dir: f.dir, Timeout: 10 * time.Second, Grace: time.Second,
				Code: "v = n\ntime.sleep(0.3)\nn = v + 1"})
			errs <- err
		})
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if r := f.run(t, Python, "n", 10*time.Second); value(r) != "2" {
		t.Fatalf("n = %s, want 2: the cells overlapped", value(r))
	}
}
