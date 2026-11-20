//go:build unix

package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// A link and a FIFO in a source folder are skipped and named, from a volume
// and from a workspace, and the regular files still arrive.
func TestFileCopy_LinksAndSpecialFilesInASourceFolderAreSkippedAndNamed(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	writeFiles(t, f.docs, map[string]string{"src/a.txt": "a"})
	if err := os.Symlink("a.txt", filepath.Join(f.docs, "src", "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(f.docs, "src", "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	f.wsPut(t, "w", "wsrc/b.txt", "b")
	dir := f.wsDir(t, "w")
	if err := os.Symlink("b.txt", filepath.Join(dir, "wsrc", "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "wsrc", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := f.mustCopy(t, map[string]any{"from": "volume:docs:src", "to": "workspace:w:fromVol"})
	if !skippedNames(out, "link", "pipe") || out["files"] != float64(1) {
		t.Errorf("volume source: %v", out)
	}
	if got, _ := f.wsGet(t, "w", "fromVol/a.txt"); got != "a" || f.wsHas(t, "w", "fromVol/link") || f.wsHas(t, "w", "fromVol/pipe") {
		t.Errorf("workspace after the copy: a=%q", got)
	}
	out = f.mustCopy(t, map[string]any{"from": "workspace:w:wsrc", "to": "volume:docs:fromWS"})
	if !skippedNames(out, "link", "pipe") || out["files"] != float64(1) {
		t.Errorf("workspace source: %v", out)
	}
	if readFile(t, filepath.Join(f.docs, "fromWS", "b.txt")) != "b" {
		t.Error("b.txt did not arrive")
	}
	if _, err := os.Lstat(filepath.Join(f.docs, "fromWS", "link")); !os.IsNotExist(err) {
		t.Error("the link was copied")
	}
}

func skippedNames(out map[string]any, names ...string) bool {
	got, _ := out["skipped"].([]any)
	var s []string
	for _, g := range got {
		s = append(s, g.(string))
	}
	for _, n := range names {
		if !slices.Contains(s, n) {
			return false
		}
	}
	return true
}

// randomReader is n pseudo-random bytes made as they are read.
type randomReader struct {
	r    *rand.ChaCha8
	left int64
}

func (z *randomReader) Read(p []byte) (int, error) {
	if z.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.left {
		p = p[:z.left]
	}
	_, _ = z.r.Read(p)
	z.left -= int64(len(p))
	return len(p), nil
}

// A 100 MiB file copied out of a workspace into a volume streams through the
// core: the process's heap (core and worker both run in it) grows by a few
// MiB, not by the file, and the bytes arrive exactly.
func TestFileCopy_A100MiBWorkspaceFileToAVolumeKeepsTheHeapSmall(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 200 MiB")
	}
	const size = 100 << 20
	f := newFCFixture(t, inbox.Service{MaxFileBytes: 200 << 20}, lifecycle.Limits{})
	f.open(t, "w")
	var seed [32]byte
	seed[0] = 7
	if _, err := f.ws.tool.Manager.PutFile(context.Background(), f.owner(), "w", workspace.PutArgs{Path: "big.bin", Size: size}, &randomReader{r: rand.NewChaCha8(seed), left: size}); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, _ = io.Copy(h, &randomReader{r: rand.NewChaCha8(seed), left: size})
	want := hex.EncodeToString(h.Sum(nil))

	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	out := f.mustCopy(t, map[string]any{"from": "workspace:w:big.bin", "to": "volume:docs:big.bin"})
	close(stop)
	<-sampled
	if out["sha256"] != want || out["size"] != float64(size) {
		t.Fatalf("copy = %v, want sha256 %s", out, want)
	}
	growth := float64(int64(peak.Load())-int64(base.HeapAlloc)) / (1 << 20)
	t.Logf("heap before %.1f MiB, growth at peak %.1f MiB (file %d MiB)", float64(base.HeapAlloc)/(1<<20), growth, size>>20)
	const bound = 16 // MiB: chunk windows and gRPC's buffers, far under the file
	if growth > bound {
		t.Errorf("the heap grew by %.1f MiB, over %d MiB", growth, bound)
	}
}

// A same-worker folder copy copies exactly the files the core listed and
// judged: a file a command adds after the listing is not copied, and a
// listed file replaced by a link, or grown, refuses the copy with nothing
// committed. Without the list the worker would re-list the folder and copy
// what no hook saw.
func TestFileCopy_TheWorkerCopiesOnlyTheListedFiles(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, dir string)
		refuse bool
	}{
		{"a file added after the listing", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "src", "late.txt"), []byte("late"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a listed file replaced by a link", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "src", "sub", "b.txt")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../a.txt", p); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a listed file grown", func(t *testing.T, dir string) {
			fh, err := os.OpenFile(filepath.Join(dir, "src", "a.txt"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fh.WriteString(" and much more")
			fh.Close()
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
			f.open(t, "a")
			f.open(t, "b")
			f.wsPut(t, "a", "src/a.txt", "alpha")
			f.wsPut(t, "a", "src/sub/b.txt", "bravo")
			dir := f.wsDir(t, "a")
			f.file.afterListing = func() { c.change(t, dir) }
			out, res := f.exec(t, f.ctx, map[string]any{"op": "copy", "from": "workspace:a:src", "to": "workspace:b:dst"})
			if c.refuse {
				if !res.IsError {
					t.Fatalf("copy = %v, want a refusal", out)
				}
				if f.wsHas(t, "b", "dst") {
					t.Error("a refused copy committed dst")
				}
				return
			}
			if res.IsError {
				t.Fatalf("copy: %s", res.Text)
			}
			if f.wsHas(t, "b", "dst/late.txt") || out["files"] != float64(2) {
				t.Errorf("the late file was copied: %v", out)
			}
			if got, _ := f.wsGet(t, "b", "dst/sub/b.txt"); got != "bravo" {
				t.Errorf("dst/sub/b.txt = %q", got)
			}
		})
	}
}
