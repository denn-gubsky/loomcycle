//go:build unix

package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// within fails the test when op does not return within d: an op that opens
// a FIFO with a blocking open never returns, and holds the workspace's write
// lock while it waits.
func within(t *testing.T, d time.Duration, what string, op func() answer) answer {
	t.Helper()
	done := make(chan answer, 1)
	go func() { done <- op() }()
	select {
	case a := <-done:
		return a
	case <-time.After(d):
		t.Fatalf("%s did not return within %v: it blocked on the FIFO", what, d)
		return answer{}
	}
}

// A FIFO in the workspace (a command can make one) is never opened: every
// op on it, or on a folder holding it, returns at once, refusing the FIFO
// itself with not_regular_file, and the workspace stays writable after.
func TestWorkspaceOps_AFIFOIsNeverOpenedAndBlocksNothing(t *testing.T) {
	w, root := newTestWorker(t)
	ws := filepath.Join(root, "ws1")
	if err := os.MkdirAll(filepath.Join(ws, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"f", "d/f"} {
		if err := syscall.Mkfifo(filepath.Join(ws, p), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "d", "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "ws2"), 0o700); err != nil {
		t.Fatal(err)
	}
	const d = 800 * time.Millisecond
	for _, c := range []struct {
		op   string
		args any
	}{
		{workspace.OpRead, workspace.ReadArgs{Path: "f"}},
		{workspace.OpEdit, workspace.EditArgs{Path: "f", OldString: "a", NewString: "b"}},
		{workspace.OpWrite, workspace.WriteArgs{Path: "f", Content: "x"}},
	} {
		a := within(t, d, c.op, func() answer { return runOp(t, w, c.op, c.args) })
		if a.class == "" {
			t.Errorf("%s of a FIFO succeeded", c.op)
		}
	}
	a := within(t, d, "grep", func() answer {
		return runOp(t, w, workspace.OpGrep, workspace.GrepArgs{Pattern: "fine", OutputMode: "files_with_matches"})
	})
	if !strings.Contains(fileText(t, a), "ok.txt") {
		t.Errorf("grep past a FIFO = %q", a.output)
	}
	a = within(t, d, "fstat", func() answer {
		return fop(t, w, "ws1", workspace.OpFStat, workspace.FStatArgs{Path: "f", SHA256: true})
	})
	var fi workspace.FileInfo
	_ = json.Unmarshal(a.output, &fi)
	if a.class != "" || fi.Type != workspace.EntryOther || fi.SHA256 != "" {
		t.Errorf("fstat of a FIFO = %s %+v, want other and no digest", a.class, fi)
	}
	a = within(t, d, "copy_local", func() answer {
		return fop(t, w, "ws2", workspace.OpCopyLocal, workspace.CopyLocalArgs{FromWorkspace: "ws1", FromPath: "d", Path: "d", MaxFiles: 10, MaxBytes: 1 << 20})
	})
	var ca workspace.CopyAnswer
	_ = json.Unmarshal(a.output, &ca)
	if a.class != "" || ca.Files != 1 || len(ca.Skipped) != 1 || ca.Skipped[0] != "f" {
		t.Errorf("copy of a folder holding a FIFO = %s %s %+v, want ok.txt copied and f skipped", a.class, a.detail, ca)
	}
	a = within(t, d, "a write after", func() answer {
		return runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "after.txt", Content: "still writable"})
	})
	if a.class != "" {
		t.Errorf("a write after the FIFO ops: %s %s", a.class, a.detail)
	}
}
