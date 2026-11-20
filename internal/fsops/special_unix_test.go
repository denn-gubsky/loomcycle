//go:build unix

package fsops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO is refused unopened by every read path, so a reader never waits
// for a writer that will not come; Grep walks past it.
func TestOpenIn_RefusesAFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "f"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := Read(context.Background(), root, "", ReadRequest{Path: "f"}); !errors.Is(err, ErrNotRegular) {
			t.Errorf("Read of a FIFO = %v, want ErrNotRegular", err)
		}
		if _, err := Edit(context.Background(), root, "", EditRequest{Path: "f", OldString: "a", NewString: "b"}); err == nil {
			t.Error("Edit of a FIFO succeeded")
		}
		if out, n, err := Grep(root, "", GrepRequest{Pattern: "needle", OutputMode: "files_with_matches"}); err != nil || n != 1 {
			t.Errorf("Grep past a FIFO = %q %d %v", out, n, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(800 * time.Millisecond):
		t.Fatal("a read path blocked on the FIFO")
	}
}
