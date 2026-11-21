//go:build unix

package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// A link or a FIFO as a source is refused on the worker, before it is
// opened for reading: the worker's own check, beneath the core's.
func TestConvert_ALinkOrAFIFOAsASourceIsRefusedOnTheWorker(t *testing.T) {
	f, o, cv := convFixture(t, Limits{MaxPerRun: 4})
	f.open(t, o, "w")
	dir := f.h.Dir(f.rowOf(t, o, "w"))
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	sha := strings.Repeat("0", 64)
	for p, class := range map[string]string{"link": workspace.ClassInvalid, "pipe": workspace.ClassNotRegular} {
		err := f.m.Convert(context.Background(), o, ConvertRequest{Op: workspace.OpConvertExtract, Args: extractArgs(p, sha), Source: "w", SHA256: sha, To: "w"}, nil, nil)
		if classOf(err) != class {
			t.Errorf("%s: %v, want %s", p, err, class)
		}
	}
	if cv.calls() != 0 {
		t.Error("a link or a FIFO was converted")
	}
}
