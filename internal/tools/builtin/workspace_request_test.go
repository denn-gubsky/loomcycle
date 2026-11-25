package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.3 Workspace op=request through the Workspace tool and a real
// host-engine worker: one HTTP request over the tunnel (P9 plan §3.2).

// echoServer answers every request with what it received, and a few paths
// with what the tests need. It logs each request's path, so a test can see
// which requests reached it.
const echoServer = `import sys, json
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def _go(self):
        print("got", self.path, flush=True)
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode()
        if self.path == "/redirect":
            self.send_response(302); self.send_header("Location", "/there"); self.send_header("Content-Length", "0"); self.end_headers(); return
        if self.path == "/big":
            data = b"x" * (2 << 20)
        elif self.path == "/bin":
            data = bytes([0xff, 0x00, 0xfe])
        else:
            data = json.dumps({"method": self.command, "path": self.path, "host": self.headers.get("Host"),
                "x_test": self.headers.get("X-Test"), "auth": self.headers.get("Authorization"), "body": body}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("X-Echo", "yes")
        self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    do_GET = do_POST = do_PUT = do_DELETE = _go
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
`

// startEcho writes and starts echoServer in the run's workspace name on a
// free port, and waits until it answers.
func startEcho(t *testing.T, f *wsFixture, ctx context.Context, name string) int {
	t.Helper()
	needProgram(t, "python3")
	src, _ := json.Marshal(echoServer)
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"write","name":%q,"path":"echo.py","content":%s}`, name, src))
	port := freeLoopbackPort(t)
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"start","name":%q,"command":"python3 echo.py %d","port":%d}`, name, port, port))
	eventually(t, "the echo server to listen", func() bool { return portListening(port) })
	return port
}

func mustRequest(t *testing.T, f *wsFixture, ctx context.Context, input string) requestAnswer {
	t.Helper()
	res := f.mustOK(t, ctx, input)
	var a requestAnswer
	if err := json.Unmarshal([]byte(res.Text), &a); err != nil {
		t.Fatalf("%s: answer %s: %v", input, res.Text, err)
	}
	return a
}

// The owner's request reaches its own server and returns the answer: the
// method, path, headers and body arrive as sent, and nothing else is added
// (no bearer).
func TestWorkspaceRequest_ReachesTheWorkspacesOwnServer(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	port := startEcho(t, f, ctx, "web")

	a := mustRequest(t, f, ctx, fmt.Sprintf(`{"op":"request","name":"web","port":%d,"method":"POST","path":"/form?q=1","headers":{"X-Test":"hi"},"body":"a=b"}`, port))
	if a.Status != 200 || a.Body == nil || a.Headers["X-Echo"] != "yes" {
		t.Fatalf("answer = %+v", a)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(*a.Body), &got); err != nil {
		t.Fatal(err)
	}
	if got["method"] != "POST" || got["path"] != "/form?q=1" || got["x_test"] != "hi" || got["body"] != "a=b" || got["auth"] != nil {
		t.Errorf("the server received %v", got)
	}
	if got["host"] != fmt.Sprintf("localhost:%d", port) {
		t.Errorf("Host = %v", got["host"])
	}

	if a := mustRequest(t, f, ctx, fmt.Sprintf(`{"op":"request","name":"web","port":%d,"method":"GET","path":"/redirect"}`, port)); a.Status != 302 || a.Headers["Location"] != "/there" {
		t.Errorf("a redirect is returned as it is, not followed: %+v", a)
	}
	big := mustRequest(t, f, ctx, fmt.Sprintf(`{"op":"request","name":"web","port":%d,"method":"GET","path":"/big"}`, port))
	if !big.Truncated || big.Body == nil || len(*big.Body) != workspace.MaxRequestBody {
		t.Errorf("a 2 MiB answer: truncated=%v len=%d", big.Truncated, len(strDeref(big.Body)))
	}
	bin := mustRequest(t, f, ctx, fmt.Sprintf(`{"op":"request","name":"web","port":%d,"method":"GET","path":"/bin"}`, port))
	if bin.Body != nil || bin.BodyBase64 == nil || *bin.BodyBase64 != base64.StdEncoding.EncodeToString([]byte{0xff, 0, 0xfe}) {
		t.Errorf("a binary answer: %+v", bin)
	}
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// One workspace cannot reach another's server through the tunnel: not a
// workspace of another user, not another workspace of the same run, and not
// by declaring the same port for a server of its own (on the host engine
// every workspace shares one loopback). The server sees none of them.
func TestWorkspaceRequest_AnotherWorkspaceCannotReachItsServer(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	alice := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, alice, `{"op":"open","name":"web"}`)
	port := startEcho(t, f, alice, "web")

	bob := f.runCtx(t, "bob", "", wsFull)
	f.mustOK(t, bob, `{"op":"open","name":"web"}`)
	f.mustOK(t, alice, `{"op":"open","name":"other"}`)
	req := func(name string) string {
		return fmt.Sprintf(`{"op":"request","name":%q,"port":%d,"method":"GET","path":"/stolen-%s"}`, name, port, name)
	}
	if c := refusalClass(f.call(t, bob, req("web"))); c != workspace.ClassPortNotServed {
		t.Errorf("another user's workspace = %q, want %s", c, workspace.ClassPortNotServed)
	}
	if c := refusalClass(f.call(t, alice, req("other"))); c != workspace.ClassPortNotServed {
		t.Errorf("another workspace of the run = %q, want %s", c, workspace.ClassPortNotServed)
	}
	// bob declares alice's port for a server of his own.
	f.mustOK(t, bob, fmt.Sprintf(`{"op":"start","name":"web","command":"sleep 600","port":%d}`, port))
	if c := refusalClass(f.call(t, bob, req("web"))); c != workspace.ClassPortNotServed {
		t.Errorf("a port another workspace's server declares too = %q, want %s", c, workspace.ClassPortNotServed)
	}
	// Nor bob's name for alice's workspace: the pin answers first.
	if c := refusalClass(f.call(t, bob, req("missing"))); c != "not_found" {
		t.Errorf("a name the run has not opened = %q, want not_found", c)
	}
	logs := f.mustOK(t, alice, `{"op":"logs","name":"web","server":"srv-1"}`)
	if strings.Contains(logs.Text, "stolen") {
		t.Errorf("alice's server received another workspace's request:\n%s", logs.Text)
	}
}

// A request names only a port a running server of the workspace declared:
// a listener on the worker's loopback that no server declared is not
// reached, nor is a port whose server was stopped. A server declaring it is
// what lets the same request through (the host engine's check is a check,
// not isolation: D-P9-9).
func TestWorkspaceRequest_ReachesOnlyAPortItsRunningServerDeclared(t *testing.T) {
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
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 4096)
			_, _ = c.Read(buf)
			_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"))
			_ = c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	req := fmt.Sprintf(`{"op":"request","name":"web","port":%d,"method":"GET","path":"/"}`, port)

	if c := refusalClass(f.call(t, ctx, req)); c != workspace.ClassPortNotServed {
		t.Errorf("an undeclared port = %q, want %s", c, workspace.ClassPortNotServed)
	}
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"start","name":"web","command":"sleep 600","port":%d}`, port+1))
	if c := refusalClass(f.call(t, ctx, req)); c != workspace.ClassPortNotServed {
		t.Errorf("a port another server of the workspace did not declare = %q", c)
	}
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"start","name":"web","command":"sleep 600","port":%d}`, port))
	f.mustOK(t, ctx, `{"op":"stop","name":"web","server":"srv-2"}`)
	if c := refusalClass(f.call(t, ctx, req)); c != workspace.ClassPortNotServed {
		t.Errorf("a port whose server was stopped = %q", c)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the listener accepted %d connections from refused requests", n)
	}
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"start","name":"web","command":"sleep 600","port":%d}`, port))
	if a := mustRequest(t, f, ctx, req); a.Status != 204 {
		t.Errorf("a declared port = %+v", a)
	}
	if accepted.Load() != 1 {
		t.Errorf("accepted = %d, want 1", accepted.Load())
	}
}

// A share never carries the tunnel: a peer holding a read_write share is
// refused request on the shared name, and a share with connect is still
// refused (connect arrives with P9 PR 7).
func TestWorkspaceRequest_AShareWithoutConnectCannotUseTheTunnel(t *testing.T) {
	f := newShareFixture(t, wsFull)
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read_write"}`)
	res := f.wsCall(t, f.peer, `{"op":"request","name":"w@a_alice","port":8000,"method":"GET","path":"/"}`)
	if c := refusalClass(res); c != "not_shared_op" {
		t.Errorf("request through a read_write share = %q %s, want not_shared_op", c, res.Text)
	}
	wantShareRefusal(t, "connect", f.wsCall(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read","connect":true}`), "not_yet")
}

// request is a command's grant (exec): an agent without it is refused
// before anything reaches the worker; another run is told not_found.
func TestWorkspaceRequest_NeedsTheExecGrantAndThePin(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"web"}`)
	in := `{"op":"request","name":"web","port":8000,"method":"GET","path":"/"}`
	readWrite := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: tools.WorkspacePolicyValue{Read: true, Write: true, Open: true}})
	if c := refusalClass(f.call(t, readWrite, in)); c != "not_granted" {
		t.Errorf("without exec = %q, want not_granted", c)
	}
	other := f.runCtx(t, "bob", "", wsFull)
	if c := refusalClass(f.call(t, other, in)); c != "not_found" {
		t.Errorf("another run = %q, want not_found", c)
	}
}

// Bad arguments are refused before the worker: the framing headers are the
// runtime's, a path must be a request target, the body is bounded.
func TestWorkspaceRequest_StrictArguments(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	long, _ := json.Marshal(strings.Repeat("x", workspace.MaxRequestBody+1))
	for _, in := range []string{
		`{"op":"request","name":"web","method":"GET","path":"/"}`,
		`{"op":"request","name":"web","port":70000,"method":"GET","path":"/"}`,
		`{"op":"request","name":"web","port":8000,"path":"/"}`,
		`{"op":"request","name":"web","port":8000,"method":"GET"}`,
		`{"op":"request","name":"web","port":8000,"method":"G ET","path":"/"}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"http://evil/"}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/a b"}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/\r\nX: y"}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/","headers":{"Content-Length":"5"}}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/","headers":{"Transfer-Encoding":"chunked"}}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/","headers":{"Connection":"keep-alive"}}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/","headers":{"X-A":"b\r\nX-C: d"}}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/","headers":{"Bad Name":"x"}}`,
		`{"op":"request","name":"web","port":8000,"method":"POST","path":"/","body":` + string(long) + `}`,
		`{"op":"request","name":"web","port":8000,"method":"GET","path":"/","command":"x"}`,
	} {
		if c := refusalClass(f.call(t, ctx, in)); c != "invalid_argument" {
			t.Errorf("%.120s = %q, want invalid_argument", in, c)
		}
	}
}

// The tunnel carries bytes both ways at once (P9 plan §3.2): what is
// written reaches the port before the input ends, and the port's answer is
// read while the input is still open. CloseWrite ends the input and the
// port's close ends the output; Close of a tunnel nobody reads returns.
func TestWorkspaceTunnel_CarriesBytesBothWaysBeforeEitherSideEnds(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"start","name":"web","command":"sleep 600","port":%d}`, port))
	ident := tools.RunIdentity(ctx)
	row, err := f.tool.Manager.Get(ctx, lifecycle.Owner{TenantID: "acme", UserID: "alice", RunID: tools.RunID(ctx), AgentID: ident.AgentID, RootRunID: ident.RootRunID}, "web")
	if err != nil {
		t.Fatal(err)
	}
	tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tun, err := f.tool.Manager.Tunnel(tctx, row, port, 1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"ping", "pong"} {
		if _, err := tun.Write([]byte(word)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(word))
		if _, err := io.ReadFull(tun, got); err != nil || string(got) != word {
			t.Fatalf("echo of %q = %q, %v", word, got, err)
		}
	}
	if err := tun.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(tun); err != nil || len(rest) != 0 {
		t.Errorf("after CloseWrite: %q, %v; want the port's close", rest, err)
	}
	_ = tun.Close()
	if err := tun.Err(); err != nil {
		t.Errorf("Err = %v, want nil: the port closed", err)
	}

	// A tunnel whose reader stops reading is ended by Close.
	tun2, err := f.tool.Manager.Tunnel(tctx, row, port, 8<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = tun2.Write(make([]byte, 4<<20)) }()
	closed := make(chan struct{})
	go func() { _ = tun2.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("Close of a tunnel nobody reads did not return")
	}
}

// A restore stops the workspace's servers, and so ends what a tunnel
// reaches; a fork starts with no server, so no tunnel reaches it until one
// is started there (P9 PR 3: the tunnel keeps no state of its own).
func TestWorkspaceRequest_NoServerAfterARestoreOrInAFork(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	port := startEcho(t, f, ctx, "web")
	req := func(name string) string {
		return fmt.Sprintf(`{"op":"request","name":%q,"port":%d,"method":"GET","path":"/"}`, name, port)
	}
	mustRequest(t, f, ctx, req("web"))
	var cp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.mustOK(t, ctx, `{"op":"checkpoint","name":"web"}`).Text), &cp); err != nil || cp.ID == "" {
		t.Fatalf("checkpoint: %v", err)
	}
	f.mustOK(t, ctx, `{"op":"fork","name":"web","as":"copy"}`)
	if c := refusalClass(f.call(t, ctx, req("copy"))); c != workspace.ClassPortNotServed {
		t.Errorf("a fork = %q, want %s", c, workspace.ClassPortNotServed)
	}
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"restore","name":"web","checkpoint":%q}`, cp.ID))
	if c := refusalClass(f.call(t, ctx, req("web"))); c != workspace.ClassPortNotServed {
		t.Errorf("after a restore = %q, want %s", c, workspace.ClassPortNotServed)
	}
}
