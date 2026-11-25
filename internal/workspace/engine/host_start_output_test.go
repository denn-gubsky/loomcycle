package engine

import (
	"context"
	"io"
	"strings"
	"testing"
)

// A started program's whole output is read even when it exits at once: the
// reap must not close the pipe under the reader (os/exec: Wait closes a
// StdoutPipe, so Wait may not run before the reads end). Seen as a server
// that failed at once and reported no output
// (TestServers_AServerThatEndsAtOnceReportsItsOutput, flaky on CI).
func TestHostStart_AProgramThatExitsAtOnceKeepsItsWholeOutput(t *testing.T) {
	h := &Host{}
	dir := t.TempDir()
	for i := range 300 {
		p, err := h.Start(context.Background(), StartRequest{Workspace: "ws1", Root: dir, Dir: dir,
			Command: "echo no such thing >&2; exit 3"})
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(p.Stdout())
		<-p.Done()
		if err != nil || !strings.Contains(string(out), "no such thing") {
			t.Fatalf("start %d: output %q, err %v; want the whole output", i, out, err)
		}
	}
}
