package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

func serverOpRun(t *testing.T, w *worker, op string, args any) answer {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws1", Op: op, Args: raw}, nil)
}

func startedServer(t *testing.T, w *worker, args workspace.ServerStartArgs) workspace.ServerInfo {
	t.Helper()
	a := serverOpRun(t, w, workspace.OpServerStart, args)
	if a.class != "" {
		t.Fatalf("start refused: %s: %s", a.class, a.detail)
	}
	var info workspace.ServerInfo
	if err := json.Unmarshal(a.output, &info); err != nil {
		t.Fatalf("answer %q: %v", a.output, err)
	}
	return info
}

func serverLogs(t *testing.T, w *worker, args workspace.ServerLogsArgs) []workspace.ServerInfo {
	t.Helper()
	a := serverOpRun(t, w, workspace.OpServerLogs, args)
	if a.class != "" {
		t.Fatalf("logs refused: %s: %s", a.class, a.detail)
	}
	var ans workspace.ServerLogsAnswer
	if err := json.Unmarshal(a.output, &ans); err != nil {
		t.Fatalf("answer %q: %v", a.output, err)
	}
	return ans.Servers
}

// freePort is a TCP port free on the loopback now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH")
	}
}

// waitFor polls cond until it holds or the time is up.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// listening reports whether anything accepts a connection on port: the
// check that a server is gone, which a 404 from a server still running
// would not be.
func listening(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func answers(port int) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/hello.txt")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode == 200 && string(b) == "hi from the workspace"
}

// A server outlives the op that started it: it answers on its port after
// start returned, logs shows what it wrote, and stop ends it (RFC GW §5.3).
func TestServers_StartedServerOutlivesTheOpAndStopEndsIt(t *testing.T) {
	requirePython(t)
	w, root := newTestWorker(t)
	if err := os.WriteFile(filepath.Join(root, "ws1", "hello.txt"), []byte("hi from the workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	info := startedServer(t, w, workspace.ServerStartArgs{Command: fmt.Sprintf("echo booting; python3 -m http.server %d --bind 127.0.0.1", port), Port: port})
	t.Cleanup(func() { w.servers.Close() })
	if !info.Running || info.Server != "srv-1" || info.Port != port {
		t.Fatalf("start = %+v, want srv-1 running on %d", info, port)
	}
	waitFor(t, "the server to answer", func() bool { return answers(port) })
	waitFor(t, "the log to show the boot line", func() bool {
		got := serverLogs(t, w, workspace.ServerLogsArgs{Server: info.Server})
		return len(got) == 1 && got[0].Output != nil && strings.Contains(*got[0].Output, "booting")
	})
	list := serverLogs(t, w, workspace.ServerLogsArgs{})
	if len(list) != 1 || list[0].Output != nil || !list[0].Running {
		t.Errorf("logs without server = %+v, want one running server and no output", list)
	}
	a := serverOpRun(t, w, workspace.OpServerStop, workspace.ServerStopArgs{Server: info.Server})
	if a.class != "" || !strings.Contains(string(a.output), `"stopped":true`) {
		t.Fatalf("stop = %s %s %s", a.class, a.detail, a.output)
	}
	waitFor(t, "the server to stop listening", func() bool { return !listening(port) })
	if got := serverLogs(t, w, workspace.ServerLogsArgs{}); len(got) != 1 || got[0].Running || got[0].EndedAt.IsZero() {
		t.Errorf("after stop logs = %+v, want it ended", got)
	}
	// A second stop finds it ended.
	if a := serverOpRun(t, w, workspace.OpServerStop, workspace.ServerStopArgs{Server: info.Server}); !strings.Contains(string(a.output), `"stopped":false`) {
		t.Errorf("second stop = %s", a.output)
	}
}

// A server and what it started end when its workspace is removed, as at a
// release or a reconciliation: nothing keeps serving a workspace that is
// gone.
func TestServers_RemovingTheWorkspaceEndsItsServersAndTheirChildren(t *testing.T) {
	requirePython(t)
	w, root := newTestWorker(t)
	if err := os.WriteFile(filepath.Join(root, "ws1", "hello.txt"), []byte("hi from the workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	// The server is a child of the shell the command line starts, so the
	// removal must end the group, not only the shell.
	startedServer(t, w, workspace.ServerStartArgs{Command: fmt.Sprintf("python3 -m http.server %d --bind 127.0.0.1 & wait", port)})
	t.Cleanup(func() { w.servers.Close() })
	waitFor(t, "the server to answer", func() bool { return answers(port) })
	if a := w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws1", Op: workspace.OpRemove}, nil); a.class != "" {
		t.Fatalf("remove refused: %s: %s", a.class, a.detail)
	}
	waitFor(t, "the server to stop listening", func() bool { return !listening(port) })
	if got := w.servers.list("ws1"); len(got) != 0 {
		t.Errorf("servers after removal = %d, want none", len(got))
	}
}

// The worker's stop ends every server.
func TestServers_CloseEndsEveryServer(t *testing.T) {
	requirePython(t)
	w, root := newTestWorker(t)
	if err := os.WriteFile(filepath.Join(root, "ws1", "hello.txt"), []byte("hi from the workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	startedServer(t, w, workspace.ServerStartArgs{Command: fmt.Sprintf("python3 -m http.server %d --bind 127.0.0.1", port)})
	waitFor(t, "the server to answer", func() bool { return answers(port) })
	w.servers.Close()
	waitFor(t, "the server to stop listening", func() bool { return !listening(port) })
}

// A workspace runs at most MaxServers at once; an ended one does not count.
func TestServers_AFifthRunningServerIsRefused(t *testing.T) {
	w, _ := newTestWorker(t)
	t.Cleanup(func() { w.servers.Close() })
	for i := range workspace.MaxServers {
		if info := startedServer(t, w, workspace.ServerStartArgs{Command: "sleep 60"}); info.Server != "srv-"+strconv.Itoa(i+1) {
			t.Fatalf("server %d = %+v", i, info)
		}
	}
	a := serverOpRun(t, w, workspace.OpServerStart, workspace.ServerStartArgs{Command: "sleep 60"})
	if a.class != workspace.ClassServerLimit {
		t.Fatalf("fifth start = %q %s, want %s", a.class, a.detail, workspace.ClassServerLimit)
	}
	if a := serverOpRun(t, w, workspace.OpServerStop, workspace.ServerStopArgs{Server: "srv-2"}); a.class != "" {
		t.Fatalf("stop: %s %s", a.class, a.detail)
	}
	if info := startedServer(t, w, workspace.ServerStartArgs{Command: "sleep 60"}); info.Server != "srv-5" {
		t.Errorf("start after a stop = %+v, want srv-5", info)
	}
}

// A server that ends at once says why in start's own answer.
func TestServers_AServerThatEndsAtOnceReportsItsOutput(t *testing.T) {
	w, _ := newTestWorker(t)
	info := startedServer(t, w, workspace.ServerStartArgs{Command: "echo no such thing >&2; exit 3"})
	if info.Running || info.Output == nil || !strings.Contains(*info.Output, "no such thing") {
		t.Errorf("start = %+v, want it ended with its output", info)
	}
}

// The operator's command list holds for a server as for exec.
func TestServers_StartIsHeldToTheCommandList(t *testing.T) {
	w, _ := newTestWorker(t, "python3")
	a := serverOpRun(t, w, workspace.OpServerStart, workspace.ServerStartArgs{Command: "sh -c 'sleep 60'"})
	if a.class != workspace.ClassRefused {
		t.Errorf("start of an unlisted command = %q %s, want %s", a.class, a.detail, workspace.ClassRefused)
	}
	if got := w.servers.list("ws1"); len(got) != 0 {
		t.Errorf("a refused start left %d servers", len(got))
	}
}

// Unknown server ids, bad ports and a cwd out of the workspace are refused.
func TestServers_RefusesBadArguments(t *testing.T) {
	w, _ := newTestWorker(t)
	for name, c := range map[string]struct {
		op   string
		args any
		want string
	}{
		"stop unknown":   {workspace.OpServerStop, workspace.ServerStopArgs{Server: "srv-9"}, workspace.ClassServerNotFound},
		"logs unknown":   {workspace.OpServerLogs, workspace.ServerLogsArgs{Server: "srv-9"}, workspace.ClassServerNotFound},
		"port too big":   {workspace.OpServerStart, workspace.ServerStartArgs{Command: "true", Port: 70000}, workspace.ClassInvalid},
		"cwd out":        {workspace.OpServerStart, workspace.ServerStartArgs{Command: "true", Cwd: ".."}, workspace.ClassInvalid},
		"empty":          {workspace.OpServerStart, workspace.ServerStartArgs{Command: "  "}, workspace.ClassInvalid},
		"tail too large": {workspace.OpServerLogs, workspace.ServerLogsArgs{TailBytes: workspace.ServerLogBytes + 1}, workspace.ClassInvalid},
	} {
		if a := serverOpRun(t, w, c.op, c.args); a.class != c.want {
			t.Errorf("%s: class %q (%s), want %q", name, a.class, a.detail, c.want)
		}
	}
}

// The ring keeps the latest bytes and says when earlier ones were dropped.
func TestRing_KeepsTheLatestBytes(t *testing.T) {
	r := &ring{max: 8}
	_, _ = r.Write([]byte("abcdef"))
	if b, dropped := r.tail(100); string(b) != "abcdef" || dropped {
		t.Errorf("tail = %q %v", b, dropped)
	}
	_, _ = r.Write([]byte("ghij"))
	if b, dropped := r.tail(100); string(b) != "cdefghij" || !dropped {
		t.Errorf("tail = %q %v, want the last 8 and dropped", b, dropped)
	}
	if b, dropped := r.tail(3); string(b) != "hij" || !dropped {
		t.Errorf("tail(3) = %q %v", b, dropped)
	}
}
