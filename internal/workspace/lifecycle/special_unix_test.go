//go:build unix

package lifecycle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// A FIFO a command made blocks nothing through the streaming path either:
// get of it, put over it, and the quota walk at the first write all return
// at once, and the workspace stays writable.
func TestFileOps_AFIFOBlocksNoStreamingOpOrTheQuotaWalk(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	if err := syscall.Mkfifo(filepath.Join(f.h.Dir(r.ID), "f"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	// The first write measures the workspace, walking past the FIFO.
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "a", Size: 1}, strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.GetFile(ctx, o, "w", workspace.GetArgs{Path: "f"}, io.Discard, 0); classOf(err) != workspace.ClassNotRegular {
		t.Errorf("get of a FIFO = %v, want not_regular_file", err)
	}
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "f", Size: 1}, strings.NewReader("x")); classOf(err) != workspace.ClassExists {
		t.Errorf("put onto a FIFO without overwrite = %v, want exists", err)
	}
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "b", Size: 1}, strings.NewReader("b")); err != nil {
		t.Errorf("a put after: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the ops took %v", d)
	}
	if _, err := os.Stat(filepath.Join(f.h.Dir(r.ID), "b")); err != nil {
		t.Error(err)
	}
}
