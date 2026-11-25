package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.2 (D20), §5.3, §7, P9 plan §3.4: a peer holding a share with
// connect, and its own connect grant, sends single HTTP requests to the
// ports the owner EXPOSED, through the same tunnel and listener check as
// the owner's request. Everything else is refused.

var (
	wsConnectOwner = tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Share: true, Expose: true, Connect: true}
	wsConnectPeer  = tools.WorkspacePolicyValue{Read: true, Connect: true}
)

type connectFixture struct {
	*shareFixture
	audit *recordingAudit
}

type recordingAudit struct {
	mu  chan struct{}
	evs []audit.Event
}

func (r *recordingAudit) Record(e audit.Event) error {
	r.mu <- struct{}{}
	r.evs = append(r.evs, e)
	<-r.mu
	return nil
}

func (r *recordingAudit) uses() []audit.Event {
	r.mu <- struct{}{}
	defer func() { <-r.mu }()
	var out []audit.Event
	for _, e := range r.evs {
		if e.Action == "workspace_share_use" {
			out = append(out, e)
		}
	}
	return out
}

// newConnectFixture: alice's owner run (a_alice) holds workspace w, exposes
// pages, and may share with connect; a_peer is another run of alice
// granted peer.
func newConnectFixture(t *testing.T, peer tools.WorkspacePolicyValue) *connectFixture {
	t.Helper()
	f := newShareFixture(t, peer)
	f.ctx = tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = wsConnectOwner })
	f.ws.tool.Manager.SetExpose(lifecycle.ExposeLimits{Enabled: true, MaxPerWorkspace: 2, IdleTimeout: 10 * time.Minute})
	rec := &recordingAudit{mu: make(chan struct{}, 1)}
	f.ws.tool.Manager.SetAudit(rec)
	return &connectFixture{shareFixture: f, audit: rec}
}

func peerRequest(port int) string {
	return fmt.Sprintf(`{"op":"request","name":"w@a_alice","port":%d,"method":"GET","path":"/hello"}`, port)
}

// The path the whole feature exists for: an exposed page answers a peer's
// request, the answer is the server's, and the use is audited naming both
// runs and the port.
func TestWorkspaceConnect_APeerWithConnectReachesAnExposedPort(t *testing.T) {
	f := newConnectFixture(t, wsConnectPeer)
	port := startEcho(t, f.ws, f.ctx, "w")
	f.ws.mustOK(t, f.ctx, fmt.Sprintf(`{"op":"expose","name":"w","port":%d}`, port))
	res := f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read","connect":true}`)
	if !strings.Contains(res.Text, `"connect":true`) {
		t.Errorf("share = %s, want it to say connect", res.Text)
	}
	a := mustRequest(t, f.ws, f.peer, peerRequest(port))
	var got map[string]any
	if a.Status != 200 || a.Body == nil || json.Unmarshal([]byte(*a.Body), &got) != nil || got["path"] != "/hello" || got["auth"] != nil {
		t.Fatalf("answer = %+v", a)
	}
	uses := f.audit.uses()
	if len(uses) != 1 || uses[0].WorkspacePeerAgentID != "a_peer" || uses[0].WorkspaceOwnerAgentID != "a_alice" ||
		!strings.Contains(uses[0].WorkspaceOp, fmt.Sprintf("request :%d", port)) {
		t.Errorf("audit = %+v, want one use naming both runs and the port", uses)
	}
}

// Deny by default on every axis the plan names: the share must carry
// connect, the peer must hold its own connect grant, the owner must hold
// connect to hand it over, and the port must be one the owner exposed, while
// the operator's switch is on.
func TestWorkspaceConnect_EveryMissingPieceRefuses(t *testing.T) {
	f := newConnectFixture(t, wsConnectPeer)
	exposed := startEcho(t, f.ws, f.ctx, "w")
	f.ws.mustOK(t, f.ctx, fmt.Sprintf(`{"op":"expose","name":"w","port":%d}`, exposed))

	// A share without connect.
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read_write"}`)
	if c := refusalClass(f.wsCall(t, f.peer, peerRequest(exposed))); c != "not_granted" {
		t.Errorf("a share without connect = %q, want not_granted", c)
	}
	// An owner without connect cannot hand it over.
	noConnect := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace.Connect = false })
	wantShareRefusal(t, "share connect by an owner without connect",
		f.wsCall(t, noConnect, `{"op":"share","name":"w","with":"a_peer","access":"read","connect":true}`), "not_granted")

	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read","connect":true}`)
	// A peer without its own connect grant.
	readOnly := tools.UpdateRunPolicy(f.peer, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true, Exec: true} })
	if c := refusalClass(f.wsCall(t, readOnly, peerRequest(exposed))); c != "not_granted" {
		t.Errorf("a peer without connect = %q, want not_granted", c)
	}
	// A port a running server listens on but the owner never exposed.
	other := startEcho(t, f.ws, f.ctx, "w")
	if c := refusalClass(f.wsCall(t, f.peer, peerRequest(other))); c != "not_exposed" {
		t.Errorf("an unexposed port = %q, want not_exposed", c)
	}
	// The connect share reaches nothing else of the workspace.
	for _, in := range []string{`{"op":"exec","name":"w@a_alice","command":"true"}`,
		fmt.Sprintf(`{"op":"expose","name":"w@a_alice","port":%d}`, other),
		`{"op":"start","name":"w@a_alice","command":"true"}`, `{"op":"logs","name":"w@a_alice"}`} {
		if c := refusalClass(f.wsCall(t, f.peer, in)); c != "not_shared_op" && c != "not_granted" {
			t.Errorf("%s through a connect share = %q, want a refusal", in, c)
		}
	}
	// The operator's switch off closes the page to the peer too.
	f.ws.tool.Manager.SetExpose(lifecycle.ExposeLimits{Enabled: false, MaxPerWorkspace: 2, IdleTimeout: 10 * time.Minute})
	if c := refusalClass(f.wsCall(t, f.peer, peerRequest(exposed))); c != "not_exposed" {
		t.Errorf("with workspace.expose off = %q, want not_exposed", c)
	}
	f.ws.tool.Manager.SetExpose(lifecycle.ExposeLimits{Enabled: true, MaxPerWorkspace: 2, IdleTimeout: 10 * time.Minute})
	mustRequest(t, f.ws, f.peer, peerRequest(exposed))
	// unexpose, then unshare.
	f.ws.mustOK(t, f.ctx, fmt.Sprintf(`{"op":"unexpose","name":"w","port":%d}`, exposed))
	if c := refusalClass(f.wsCall(t, f.peer, peerRequest(exposed))); c != "not_exposed" {
		t.Errorf("after unexpose = %q, want not_exposed", c)
	}
	f.ws.mustOK(t, f.ctx, fmt.Sprintf(`{"op":"expose","name":"w","port":%d}`, exposed))
	f.ws.mustOK(t, f.ctx, `{"op":"unshare","name":"w","with":"a_peer"}`)
	if c := refusalClass(f.wsCall(t, f.peer, peerRequest(exposed))); c != "not_found" {
		t.Errorf("after unshare = %q, want not_found", c)
	}
	if n := len(f.audit.uses()); n != 1 {
		t.Errorf("%d uses audited, want only the one request that went through", n)
	}
}

// An exposed port is still dialled only through the #2917 check: a page
// whose port a running server of the workspace declared but something else
// listens on is refused port_not_served, and that listener sees nothing.
func TestWorkspaceConnect_AnExposedPortIsStillCheckedAtTheDial(t *testing.T) {
	needProgram(t, "python3")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	held := l.Addr().(*net.TCPAddr).Port
	f := newConnectFixture(t, wsConnectPeer)
	f.ws.mustOK(t, f.ctx, fmt.Sprintf(`{"op":"start","name":"w","command":"sleep 600","port":%d}`, held))
	f.ws.mustOK(t, f.ctx, fmt.Sprintf(`{"op":"expose","name":"w","port":%d}`, held))
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read","connect":true}`)
	if c := refusalClass(f.wsCall(t, f.peer, peerRequest(held))); c != workspace.ClassPortNotServed {
		t.Errorf("an exposed port the worker's own process holds = %q, want %s", c, workspace.ClassPortNotServed)
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("the listener accepted %d connections from a refused request", n)
	}
}

// A share ends with its owner: a peer of an ended owner reaches nothing,
// connect included.
func TestWorkspaceConnect_EndsWithTheOwner(t *testing.T) {
	f := newConnectFixture(t, wsConnectPeer)
	port := startEcho(t, f.ws, f.ctx, "w")
	f.ws.mustOK(t, f.ctx, fmt.Sprintf(`{"op":"expose","name":"w","port":%d}`, port))
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read","connect":true}`)
	mustRequest(t, f.ws, f.peer, peerRequest(port))
	if err := f.ws.st.FinishRun(context.Background(), f.runID, store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if c := refusalClass(f.wsCall(t, f.peer, peerRequest(port))); c != "not_found" {
		t.Errorf("after the owner ended = %q, want not_found", c)
	}
}

// connect narrows at spawn as expose does: a child holds it only when both
// its parent and its own definition do.
func TestWorkspaceConnect_NarrowsAtSpawn(t *testing.T) {
	if tools.NarrowWorkspace(wsConnectOwner, wsFull).Connect || tools.NarrowWorkspace(wsFull, wsConnectOwner).Connect {
		t.Error("a child got connect that one side lacks")
	}
	if !tools.NarrowWorkspace(wsConnectOwner, wsConnectPeer).Connect {
		t.Error("connect both sides hold was dropped")
	}
}

// A spawn's share entry may carry connect only when the parent holds it,
// and the entry carries it to the child's share.
func TestSpawnWorkspaces_ConnectNeedsTheParentsConnectGrant(t *testing.T) {
	yes := true
	sharer := spawnWSCtx(tools.WorkspacePolicyValue{Read: true, Share: true})
	if _, res, ok := spawnWorkspaces(sharer, "workspaces", []spawnWorkspace{{Name: "work", Access: "read", Connect: &yes}}); ok || !strings.Contains(res.Text, "workspace.connect") {
		t.Errorf("connect without the grant = ok %v %s, want a refusal naming workspace.connect", ok, res.Text)
	}
	connector := spawnWSCtx(tools.WorkspacePolicyValue{Read: true, Share: true, Connect: true})
	got, res, ok := spawnWorkspaces(connector, "workspaces", []spawnWorkspace{{Name: "work", Access: "read", Connect: &yes}, {Name: "notes", Access: "read"}})
	if !ok {
		t.Fatalf("refused: %s", res.Text)
	}
	want := []tools.SpawnWorkspace{{Name: "work", Access: "read", Connect: true}, {Name: "notes", Access: "read"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shares = %+v, want %+v", got, want)
	}
}
