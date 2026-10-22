package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A file written to a read-only volume lives in an in-memory overlay that
// shares loomcycle's heap with every other run, so its size is capped per
// call. Writing past the default cap (64 MiB) stops with "no space left on
// device" instead of growing the heap. gbash stops any single file at its own
// 8 MiB limit (gbashFileLimit), so the cap is reached here across nine files
// of 8 MB each, every one under that limit. The total is bounded (72 MB), so
// the test cannot exhaust the machine's memory even where the cap is missing.
func TestBashboxEscape_RoOverlayWriteStopsAtTheDefaultCap(t *testing.T) {
	root := realDir(t)
	b := &Bashbox{Enabled: true}
	res := runBashbox(t, b, roCtx(root), "for i in 1 2 3 4 5 6 7 8 9; do head -c 8000000 /dev/zero > f$i || break; done")
	if !strings.Contains(res.Text, "no space left on device") {
		t.Errorf("72 MB written to a read-only volume was not stopped at the 64 MiB cap: %.300q", res.Text)
	}
}

// gbashFileLimit is gbash's default per-file size limit (policy MaxFileBytes,
// 8 MiB), which v0.0.41 also applies to a file written through a redirect.
// Loomcycle does not override it, so it is what a Bashbox script sees.
const gbashFileLimit = 8 << 20

// One file stops at gbash's per-file limit, before any overlay cap, and keeps
// what fits. On a read-only volume that bounds what one redirect can put on
// the shared heap; the 100 MB write here leaves at most 8 MiB behind.
func TestBashboxEscape_OneRoFileStopsAtTheGbashFileLimit(t *testing.T) {
	res := runBashbox(t, &Bashbox{Enabled: true}, roCtx(realDir(t)), "head -c 100000000 /dev/zero > big; echo size=$(wc -c < big)")
	m := sizeMarker.FindStringSubmatch(res.Text)
	if m == nil {
		t.Fatalf("no size=<bytes> marker: the file grew past what gbash can read back, or the script failed: %.300q", res.Text)
	}
	if got, _ := strconv.Atoi(m[1]); got > gbashFileLimit {
		t.Errorf("one redirect to a read-only volume stored %d bytes, past gbash's %d-byte file limit", got, gbashFileLimit)
	}
}

// The same limit holds on a read-write volume, where the file reaches the
// host: a model cannot write one file larger than 8 MiB through Bashbox.
// Reads through the read-write mount were already capped, at 10 MiB.
func TestBashbox_OneRwFileStopsAtTheGbashFileLimit(t *testing.T) {
	root := realDir(t)
	res := runBashbox(t, &Bashbox{Enabled: true}, bashCtx(root), "head -c 100000000 /dev/zero > big")
	fi, err := os.Stat(filepath.Join(root, "big"))
	if err != nil {
		t.Fatalf("stat: %v (result %.300q)", err, res.Text)
	}
	if fi.Size() > gbashFileLimit {
		t.Errorf("one redirect to a read-write volume wrote %d bytes to the host, past gbash's %d-byte file limit", fi.Size(), gbashFileLimit)
	}
}

// sizeMarker finds the `size=<bytes>` line a script prints; an empty count
// (wc refused the file) does not match.
var sizeMarker = regexp.MustCompile(`size=(\d+)`)

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

// peakHeapDuring runs fn and reports how far the heap rose above where it
// started while fn ran.
func peakHeapDuring(fn func()) int64 {
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
	fn()
	close(stop)
	<-done
	return int64(peak.Load()) - int64(base.HeapInuse)
}

// A change to a file of the read-only volume (chmod, touch, mv, ln, an
// append) copies it into the in-memory layer, and gbash reads the copy's
// content in the first time it is opened. Those bytes are charged as they are
// read, so a copy bigger than the cap is refused instead of held in memory.
func TestBashboxEscape_RoCopyUpOfALargeFileIsCharged(t *testing.T) {
	for _, script := range []string{
		"chmod 600 big; cat big > /dev/null",
		"touch big; cat big > /dev/null",
		"mv big moved; cat moved > /dev/null",
		"ln big hard; cat hard > /dev/null",
		"echo x >> big",
	} {
		t.Run(script, func(t *testing.T) {
			root := realDir(t)
			if err := os.WriteFile(filepath.Join(root, "big"), make([]byte, 4<<20), 0o644); err != nil {
				t.Fatal(err)
			}
			b := &Bashbox{Enabled: true, OverlayMaxBytes: 1 << 20}
			res := runBashbox(t, b, roCtx(root), script)
			if !res.IsError || !strings.Contains(res.Text, "write limit reached") {
				t.Errorf("copying a 4 MiB file into a 1 MiB overlay was not refused: %.300q", res.Text)
			}
		})
	}
}

// The attack the copy-up charge stops: touch each large file on the
// read-only volume, then read it, so every one is held in memory. Sixteen
// 4 MiB files are 64 MiB of heap uncapped; with a 1 MiB cap the first copy
// is refused and the heap stays small.
func TestBashboxEscape_RoCopyUpThenReadKeepsTheHeapBounded(t *testing.T) {
	root := realDir(t)
	for i := 0; i < 16; i++ {
		if err := os.WriteFile(filepath.Join(root, "f"+strconv.Itoa(i)), make([]byte, 4<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := &Bashbox{Enabled: true, OverlayMaxBytes: 1 << 20}
	var res tools.Result
	grew := peakHeapDuring(func() {
		res = runBashbox(t, b, roCtx(root), "for f in f*; do chmod 600 $f; cat $f > /dev/null; done")
	})
	if !strings.Contains(res.Text, "write limit reached") {
		t.Errorf("the copies were not charged: %.300q", res.Text)
	}
	if grew > 32<<20 {
		t.Errorf("heap grew %d MiB while copying 64 MiB of files into a 1 MiB overlay", grew>>20)
	}
}

// The charge is for the bytes a copy actually reads, not the size the file
// had when it was copied: gbash defers the read, and the volume's file may
// grow in between (another run writing to the same directory). Here it is
// 1 byte at the chmod and 4 MiB by the read. The script waits for a marker
// the test creates only after growing the file, so the read always sees it
// grown.
func TestBashboxEscape_RoCopyUpChargesTheBytesActuallyRead(t *testing.T) {
	root := realDir(t)
	f := filepath.Join(root, "grows")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	grown := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond) // usually after the chmod; the marker makes order irrelevant to the outcome
		if err := os.WriteFile(f, make([]byte, 4<<20), 0o644); err != nil {
			grown <- err
			return
		}
		grown <- os.WriteFile(filepath.Join(root, "grown"), nil, 0o644)
	}()
	b := &Bashbox{Enabled: true, OverlayMaxBytes: 1 << 20}
	res := runBashbox(t, b, roCtx(root), "chmod 600 grows; until [ -e grown ]; do sleep 0.05; done; cat grows > /dev/null")
	if err := <-grown; err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "write limit reached") {
		t.Errorf("a copy that grew to 4 MiB after it was charged was not refused: %.300q", res.Text)
	}
}

// Every copy is charged, whatever name reaches it: two hard links to one
// file of the volume become two copies in memory, and a file copied, removed
// and copied again under the same name is charged again. 3 MiB files, 5 MiB
// cap: either script holds 6 MiB.
func TestBashboxEscape_RoCopyUpThroughAliasesIsChargedPerCopy(t *testing.T) {
	for _, script := range []string{
		"chmod 600 a; chmod 600 b; cat a b > /dev/null",
		"chmod 600 a; cat a > /dev/null; mv a kept; mv b a; chmod 600 a; cat a > /dev/null",
	} {
		t.Run(script, func(t *testing.T) {
			root := realDir(t)
			a := filepath.Join(root, "a")
			if err := os.WriteFile(a, make([]byte, 3<<20), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(a, filepath.Join(root, "b")); err != nil {
				t.Fatal(err)
			}
			b := &Bashbox{Enabled: true, OverlayMaxBytes: 5 << 20}
			res := runBashbox(t, b, roCtx(root), script)
			if !res.IsError || !strings.Contains(res.Text, "write limit reached") {
				t.Errorf("6 MiB of copies into a 5 MiB overlay was not refused: %.300q", res.Text)
			}
		})
	}
}

// Reading the read-only volume streams from disk and is never charged: only
// what is kept in memory counts. 64 MiB read under a 1 MiB cap succeeds.
func TestBashbox_RoReadsOfTheVolumeAreNotCharged(t *testing.T) {
	root := realDir(t)
	for i := 0; i < 16; i++ {
		if err := os.WriteFile(filepath.Join(root, "f"+strconv.Itoa(i)), make([]byte, 4<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := &Bashbox{Enabled: true, OverlayMaxBytes: 1 << 20}
	res := runBashbox(t, b, roCtx(root), "for f in f*; do cat $f > /dev/null; done; echo read")
	if res.IsError || !strings.Contains(res.Text, "read") {
		t.Errorf("reading the volume was charged: %.300q", res.Text)
	}
}

// Every entry in gbash's in-memory filesystem costs heap beyond its name
// (about 700 bytes: the node, a map key holding the full path, the parent's
// child entry), so creating many empty files or directories must reach the
// cap long before the names alone would. 100k entries under a 4 MiB cap.
func TestBashboxEscape_ManyEntriesHitTheCap(t *testing.T) {
	for _, script := range []string{
		"touch {1..100000}",
		"mkdir {1..100000}",
		"for i in {1..9000}; do ln -s x l$i; done",
	} {
		t.Run(script, func(t *testing.T) {
			res := runBashbox(t, overlayCapped(), roCtx(realDir(t)), script)
			if !res.IsError || !strings.Contains(res.Text, "write limit reached") {
				t.Errorf("creating many entries did not reach a 4 MiB cap: %.300q", res.Text)
			}
		})
	}
}

// A writer whose reader never reads must not grow the heap. gbash v0.0.41
// bounds a named pipe at 64 KiB and blocks the writer, as a host pipe does;
// v0.0.40 buffered every write, and loomcycle's charge on FIFO writes is what
// stopped it there. Either layer holds the heap, so the test asserts the
// heap, not which layer refused. The writer is bounded (20 MB), so the test
// stays small even where both are missing.
func TestBashboxEscape_WriterToAnUnreadFifoKeepsTheHeapBounded(t *testing.T) {
	var res tools.Result
	grew := peakHeapDuring(func() {
		res = runBashbox(t, overlayCapped(), roCtx(realDir(t)),
			"mkfifo p; head -c 20000000 /dev/zero > p | sleep 1 < p; echo written")
	})
	if !strings.Contains(res.Text, "written") {
		t.Errorf("the script did not finish after its reader closed the FIFO: %.300q", res.Text)
	}
	if grew > 64<<20 {
		t.Errorf("heap grew %d MiB writing to a never-read FIFO under a 4 MiB cap", grew>>20)
	}
}
