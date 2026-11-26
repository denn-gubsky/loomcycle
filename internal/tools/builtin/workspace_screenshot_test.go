package builtin

import (
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.8 Workspace op=screenshot through the Workspace tool. The
// fixture's worker runs the host engine with no browser, so a call that
// passes the tool's checks is refused by the core's own check of the worker;
// the picture itself is the worker's tests' and the container engine's.

var wsScreenshotter = tools.WorkspacePolicyValue{Open: true, Read: true, Exec: true, Connect: true}

// A screenshot loads a server's page, so it needs connect (RFC GW §5.8),
// not exec or read alone.
func TestWorkspaceScreenshot_NeedsTheConnectGrant(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", tools.WorkspacePolicyValue{Open: true, Read: true, Write: true, Exec: true})
	f.mustOK(t, ctx, `{"op":"open","name":"w"}`)
	if c := refusalClass(f.call(t, ctx, `{"op":"screenshot","name":"w","port":8000}`)); c != "not_granted" {
		t.Errorf("screenshot without connect = %q, want not_granted", c)
	}
}

// Only a path of the workspace's own server, and a viewport within the
// bound, reach the core: an external URL, a path that does not start with
// /, and an over-large viewport are refused by the tool first.
func TestWorkspaceScreenshot_RefusesAnythingButALocalPathFirst(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsScreenshotter)
	f.mustOK(t, ctx, `{"op":"open","name":"w"}`)
	for _, in := range []string{
		`{"op":"screenshot","name":"w","port":8000,"path":"http://evil.example.com/"}`,
		`{"op":"screenshot","name":"w","port":8000,"path":"evil.example.com"}`,
		`{"op":"screenshot","name":"w","port":8000,"path":"//evil.example.com/"}`,
		`{"op":"screenshot","name":"w","port":0}`,
		`{"op":"screenshot","name":"w","port":8000,"width":1921}`,
		`{"op":"screenshot","name":"w","port":8000,"height":1081}`,
		`{"op":"screenshot","name":"w","port":8000,"url":"http://evil.example.com/"}`,
	} {
		if c := refusalClass(f.call(t, ctx, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
}

// A worker that does not report the browser (here a host-engine worker) is
// never asked: the core refuses before the op is sent.
func TestWorkspaceScreenshot_RefusedOnAWorkerWithoutTheBrowser(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsScreenshotter)
	f.mustOK(t, ctx, `{"op":"open","name":"w"}`)
	if c := refusalClass(f.call(t, ctx, `{"op":"screenshot","name":"w","port":8000,"path":"/"}`)); c != "browser_unavailable" {
		t.Errorf("screenshot on a worker without a browser = %q, want browser_unavailable", c)
	}
	// The worker refuses it too; the core's own check is what keeps the op
	// from being sent at all.
	for k := range f.h.D.Stats().Ops {
		if strings.HasPrefix(k, "screenshot/") {
			t.Errorf("the op reached the worker (%s)", k)
		}
	}
}

// A screenshot is the owner's: never through a share, and never over the
// MCP server (deny by default; the RFC names neither).
func TestWorkspaceScreenshot_NotThroughAShareNorOverMCP(t *testing.T) {
	if workspaceSharedOps["screenshot"] {
		t.Error("screenshot is a shared op")
	}
	if slices.Contains(WorkspaceMCPOps, "screenshot") {
		t.Error("screenshot is offered over the MCP server")
	}
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsScreenshotter)
	if c := refusalClass(f.call(t, ctx, `{"op":"screenshot","name":"w@a_bob","port":8000}`)); c != "not_shared_op" {
		t.Errorf("screenshot of a shared name = %q, want not_shared_op", c)
	}
}
