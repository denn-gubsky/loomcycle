package builtin

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A file written to a read-only volume lives in an in-memory overlay that
// shares loomcycle's heap with every other run, so its size is capped per
// call. Writing past the default cap (64 MiB) stops with "no space left on
// device" instead of growing the heap. The write here is bounded (100 MB), so
// the test cannot exhaust the machine's memory even where the cap is missing.
func TestBashboxEscape_RoOverlayWriteStopsAtTheDefaultCap(t *testing.T) {
	root := realDir(t)
	b := &Bashbox{Enabled: true}
	res := runBashbox(t, b, roCtx(root), "head -c 100000000 /dev/zero > big; wc -c < big")
	if !strings.Contains(res.Text, "no space left on device") {
		t.Errorf("a 100 MB write to a read-only volume was not stopped at the cap: %.300q", res.Text)
	}
}

// overlayCapped is a Bashbox with a small cap, so a test that writes past it
// costs megabytes, never the gigabytes an uncapped overlay would take.
func overlayCapped() *Bashbox { return &Bashbox{Enabled: true, OverlayMaxBytes: 4 << 20} }

// The headline case: a write that never stops. Without the cap `yes > f`
// passed 3 GiB of heap in about a second; with it, the call fails fast,
// says why, and the heap barely moves.
func TestBashboxEscape_UnboundedRoWriteFailsFastWithBoundedHeap(t *testing.T) {
	root := realDir(t)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	body, _ := json.Marshal(map[string]any{"command": "yes > f", "timeout_seconds": 20})
	start := time.Now()
	res, err := overlayCapped().Execute(roCtx(root), body)
	elapsed := time.Since(start)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "no space left on device") || !strings.Contains(res.Text, "write limit reached") {
		t.Errorf("want a failed call naming the limit; got %.300q", res.Text)
	}
	if strings.Contains(res.Text, "[exit: 124]") {
		t.Errorf("the timeout stopped it after %v, not the cap", elapsed)
	}
	// 4 MiB written costs about 10 MiB of heap; 128 MiB leaves room for the
	// rest of the process without letting an uncapped write pass.
	if grew := int64(peak.Load()) - int64(base.HeapInuse); grew > 128<<20 {
		t.Errorf("heap grew %d MiB during a call capped at 4 MiB", grew>>20)
	}
}

// Every way into the in-memory overlay is charged against the one budget:
// separate files, copies, appends, and symlinks whose target is a long
// string. Each script stays under 16 MiB of real writes.
func TestBashboxEscape_EveryRoWritePathSharesTheCap(t *testing.T) {
	for _, script := range []string{
		"head -c 3000000 /dev/zero > a; head -c 3000000 /dev/zero > b",
		"head -c 3000000 /dev/zero > a; cp a b; cp a c",
		"for i in 1 2 3 4 5; do head -c 1000000 /dev/zero >> a; done",
		`x=$(head -c 1000000 /dev/zero | tr '\0' a); for i in 1 2 3 4 5; do ln -s "$x" l$i; done`,
		`x=$(head -c 1000000 /dev/zero | tr '\0' a); for i in 1 2 3 4 5; do printf %s "$x" > f$i; done`,
	} {
		t.Run(script, func(t *testing.T) {
			res := runBashbox(t, overlayCapped(), roCtx(realDir(t)), script)
			if !res.IsError || !strings.Contains(res.Text, "write limit reached") {
				t.Errorf("5 MB or more into a 4 MiB overlay was not stopped: %.300q", res.Text)
			}
		})
	}
}

// Writes under the cap behave as before, so ordinary scratch work on a
// read-only volume is untouched.
func TestBashbox_RoOverlayWritesUnderTheCapSucceed(t *testing.T) {
	res := runBashbox(t, overlayCapped(), roCtx(realDir(t)),
		"head -c 3000000 /dev/zero > a; mkdir -p d/e; echo hi > d/e/f; cat d/e/f; wc -c < a")
	if res.IsError || !strings.Contains(res.Text, "hi") || !strings.Contains(res.Text, "3000000") {
		t.Errorf("a 3 MB write under a 4 MiB cap failed: %.300q", res.Text)
	}
}

// On a read-write volume with a dynamic-volume root inside it, that root is
// shadowed by an empty in-memory directory, which takes writes too: the cap
// covers it. A write elsewhere in the volume goes to disk and is not charged.
func TestBashboxEscape_ShadowMountWriteStopsAtTheCap(t *testing.T) {
	root := realDir(t)
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true, Fence: filepath.Join(root, "dyn")})
	res := runBashbox(t, overlayCapped(), ctx, "head -c 9000000 /dev/zero > dyn/big")
	if !res.IsError || !strings.Contains(res.Text, "write limit reached") {
		t.Errorf("a 9 MB write into the shadowed root was not stopped: %.300q", res.Text)
	}
	res = runBashbox(t, overlayCapped(), ctx, "head -c 6000000 /dev/zero > ondisk; wc -c < ondisk")
	if res.IsError || !strings.Contains(res.Text, "6000000") {
		t.Errorf("a write to the read-write volume itself was charged: %.300q", res.Text)
	}
}
