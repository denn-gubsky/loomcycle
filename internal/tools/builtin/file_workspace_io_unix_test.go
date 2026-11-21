//go:build unix

package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// An image link that names a link or a FIFO in the workspace is not
// imported, though it is relative and inside the workspace: the import
// reads regular files only, and the rest of the document still imports.
func TestFileImport_AnImageLinkToALinkOrAFIFOIsNotImported(t *testing.T) {
	f, d := ioFixture(t, lifecycle.Limits{})
	f.wsPutBytes(t, "w", "real.png", testPNG(t))
	dir := f.wsDir(t, "w")
	if err := os.Symlink("real.png", filepath.Join(dir, "link.png")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.png"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	f.wsPut(t, "w", "doc.md", "# Doc\n\n## L\n\n![L](link.png)\n\n## P\n\n![P](pipe.png)\n\n## R\n\n![R](real.png)\n")
	out, res := f.exec(t, f.ctx, map[string]any{"op": "import", "path": "workspace:w:doc.md"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if asInt(out["image_chunks"]) != 1 {
		t.Fatalf("image_chunks = %v, want only the regular file: %v", out["image_chunks"], out)
	}
	skipped := fmt.Sprint(out["skipped_images"])
	if !strings.Contains(skipped, "link.png") || !strings.Contains(skipped, "pipe.png") {
		t.Errorf("skipped_images = %s, want link.png and pipe.png named", skipped)
	}
	if c, _, _ := importedChunk(t, d, f.ctx, asStr(out["document_id"]), "L"); c["type"] == "image" {
		t.Errorf("the link became an image chunk: %v", c)
	}
}

// A 64 MiB upload from a workspace is served by ranged gets: the process's
// heap (core and worker both run in it) grows by a few MiB, not by the file,
// no temporary file appears on the core, and the server gets the bytes.
func TestFileUpload_A64MiBWorkspaceFileIsNotSpooledOrHeldOnTheCore(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 128 MiB")
	}
	const size = 64 << 20
	f, _ := ioFixture(t, lifecycle.Limits{})
	var seed [32]byte
	seed[0] = 11
	if _, err := f.ws.tool.Manager.PutFile(context.Background(), f.owner(), "w", workspace.PutArgs{Path: "big.bin", Size: size}, &randomReader{r: rand.NewChaCha8(seed), left: size}); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, _ = io.Copy(h, &randomReader{r: rand.NewChaCha8(seed), left: size})
	want := hex.EncodeToString(h.Sum(nil))

	var got atomic.Value
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.New()
		c, _ := io.Copy(sum, r.Body)
		n.Store(c)
		got.Store(hex.EncodeToString(sum.Sum(nil)))
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	scratch, tmp := t.TempDir(), t.TempDir()
	f.file.ScratchDir = scratch
	t.Setenv("TMPDIR", tmp)

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
	out, res := f.exec(t, f.ctx, map[string]any{"op": "upload", "from": "workspace:w:big.bin", "url": srv.URL + "/up"})
	close(stop)
	<-sampled
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["size"] != float64(size) || n.Load() != size || got.Load() != want {
		t.Fatalf("the server got %d bytes (%v), want %d (%s); result %v", n.Load(), got.Load(), size, want, out)
	}
	growth := float64(int64(peak.Load())-int64(base.HeapAlloc)) / (1 << 20)
	t.Logf("heap growth at peak %.1f MiB for a %d MiB upload", growth, size>>20)
	const bound = 16 // MiB: a read window and gRPC's buffers, far under the file
	if growth > bound {
		t.Errorf("the heap grew by %.1f MiB, over %d MiB", growth, bound)
	}
	for _, d := range []string{scratch, tmp} {
		if ents, _ := os.ReadDir(d); len(ents) != 0 {
			t.Errorf("a temporary file was written on the core in %s: %v", d, ents)
		}
	}
}
