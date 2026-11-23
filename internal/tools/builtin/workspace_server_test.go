package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.3 servers through the Workspace tool and a real host-engine
// worker.

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func portListening(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A server started in a run's workspace keeps running after the call, and
// the run's release of the workspace ends it.
func TestWorkspaceServer_StartsAndEndsWithTheWorkspacesRelease(t *testing.T) {
	needProgram(t, "python3")
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	port := freeLoopbackPort(t)
	res := f.mustOK(t, ctx, fmt.Sprintf(`{"op":"start","name":"web","command":"python3 -m http.server %d --bind 127.0.0.1","port":%d}`, port, port))
	var info workspace.ServerInfo
	if err := json.Unmarshal([]byte(res.Text), &info); err != nil || info.Server != "srv-1" || !info.Running || info.Port != port {
		t.Fatalf("start = %s (%v)", res.Text, err)
	}
	eventually(t, "the server to listen", func() bool { return portListening(port) })
	logs := f.mustOK(t, ctx, `{"op":"logs","name":"web"}`)
	if logs.Count == nil || *logs.Count != 1 || !strings.Contains(logs.Text, `"running":true`) {
		t.Errorf("logs = %s", logs.Text)
	}
	f.mustOK(t, ctx, `{"op":"release","name":"web"}`)
	eventually(t, "the server to stop listening after release", func() bool { return !portListening(port) })
}

// stop ends a server; a server that ends at once is reported as an error
// with its output.
func TestWorkspaceServer_StopAndAFailedStart(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	f.mustOK(t, ctx, `{"op":"start","name":"web","command":"sleep 60"}`)
	if res := f.mustOK(t, ctx, `{"op":"stop","name":"web","server":"srv-1"}`); !strings.Contains(res.Text, `"stopped":true`) {
		t.Errorf("stop = %s", res.Text)
	}
	res := f.call(t, ctx, `{"op":"start","name":"web","command":"echo cannot bind >&2; exit 1"}`)
	if !res.IsError || !strings.Contains(res.Text, "cannot bind") {
		t.Errorf("a start that ended at once = %v %s, want an error carrying its output", res.IsError, res.Text)
	}
	if res := f.call(t, ctx, `{"op":"stop","name":"web","server":"srv-9"}`); !res.IsError || !strings.Contains(res.Text, workspace.ClassServerNotFound) {
		t.Errorf("stop of an unknown server = %s", res.Text)
	}
}

// Servers are commands: an agent without exec can neither start one nor
// read its output, and another run cannot reach the workspace at all.
func TestWorkspaceServer_NeedsTheExecGrantAndThePin(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"web"}`)
	reader := f.runCtx(t, "alice", "", tools.WorkspacePolicyValue{Read: true, Write: true, Open: true})
	f.mustOK(t, reader, `{"op":"open","name":"web"}`)
	for _, in := range []string{`{"op":"start","name":"web","command":"sleep 60"}`, `{"op":"stop","name":"web","server":"srv-1"}`, `{"op":"logs","name":"web"}`} {
		if res := f.call(t, reader, in); !res.IsError || !strings.Contains(res.Text, "not_granted") {
			t.Errorf("%s without exec = %s, want not_granted", in, res.Text)
		}
	}
	f.mustOK(t, owner, `{"op":"start","name":"web","command":"sleep 60"}`)
	other := f.runCtx(t, "bob", "", wsFull)
	for _, in := range []string{`{"op":"logs","name":"web"}`, `{"op":"stop","name":"web","server":"srv-1"}`} {
		if res := f.call(t, other, in); !res.IsError || !strings.Contains(res.Text, "not_found") {
			t.Errorf("%s from another run = %s, want not_found", in, res.Text)
		}
	}
	// A share never reaches a server (D20: never commands).
	if res := f.call(t, owner, `{"op":"logs","name":"web@a_alice"}`); !res.IsError {
		t.Errorf("logs on a shared name = %s, want refused", res.Text)
	}
	_ = context.Background()
}

// Arguments another op takes, and bad values, are refused before the
// worker.
func TestWorkspaceServer_StrictArguments(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	for _, in := range []string{
		`{"op":"start","name":"web"}`,
		`{"op":"start","name":"web","command":"true","port":70000}`,
		`{"op":"start","name":"web","command":"true","timeout_s":5}`,
		`{"op":"stop","name":"web"}`,
		`{"op":"logs","name":"web","tail_bytes":10}`,
		`{"op":"exec","name":"web","command":"true","server":"srv-1"}`,
	} {
		if res := f.call(t, ctx, in); !res.IsError || !strings.Contains(res.Text, "invalid_argument") {
			t.Errorf("%s = %s, want invalid_argument", in, res.Text)
		}
	}
}
