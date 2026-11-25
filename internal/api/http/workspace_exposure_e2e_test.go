package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// lockedBuf is a buffer a child's output goroutine writes while the test
// reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitListening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d never listened", port)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Through a real host-engine worker: an agent with the expose grant serves a
// page its server writes, and its user reads it at the base_path expose
// returned. A dummy server that only declares a port another host process
// holds gets a 502 through the route, and that process accepts nothing: the
// route reaches a port only through the worker's bound-port check (#2917).
func TestExposureRoute_EndToEndThroughAHostWorker(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 on PATH")
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := workspacetest.Start(t, st, "acme", worker.Options{Capacity: 4})
	m := lifecycle.New(h.D, st, lifecycle.Limits{MaxPerRun: 4}, nil)
	m.SetExpose(lifecycle.ExposeLimits{Enabled: true})
	tool := &builtin.Workspace{Manager: m, ExecDefault: 10 * time.Second, ExecMax: 20 * time.Second}
	bg := context.Background()
	sess, err := st.CreateSession(bg, "acme", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(bg, sess.ID, store.RunIdentity{AgentID: "a_alice", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithRunIdentity(bg, tools.RunIdentityValue{UserID: "alice", AgentID: "a_alice", TenantID: "acme", RootRunID: run.ID})
	ctx = tools.WithRunID(ctx, run.ID)
	ctx = tools.WithRunPolicy(ctx, tools.RunPolicy{Tools: []string{"Workspace"},
		Workspace: tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Expose: true}})
	call := func(in string) string {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(in))
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", in, err, res.Text)
		}
		return res.Text
	}
	expose := func(port int) string {
		t.Helper()
		var out struct {
			BasePath string `json:"base_path"`
		}
		if err := json.Unmarshal([]byte(call(fmt.Sprintf(`{"op":"expose","name":"web","port":%d,"title":"t"}`, port))), &out); err != nil || out.BasePath == "" {
			t.Fatalf("expose: %v %+v", err, out)
		}
		return out.BasePath
	}
	call(`{"op":"open","name":"web"}`)
	call(`{"op":"write","name":"web","path":"index.html","content":"<p>made by the agent</p>"}`)
	port := freeTCPPort(t)
	call(fmt.Sprintf(`{"op":"start","name":"web","command":"python3 -m http.server %d --bind 127.0.0.1","port":%d}`, port, port))
	waitListening(t, port)
	base := expose(port)

	mux := http.NewServeMux()
	mux.Handle(exposureRoutePattern, withRequestPrincipal(newExposureHandler(m, exposeRouteLimits{MaxConnections: 8, RequestsPerSecond: 100, BytesPerSecond: 1 << 30})))
	get := func(p *auth.Principal, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer lc_test")
		r = r.WithContext(auth.WithPrincipal(r.Context(), *p))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := get(alice, base+"index.html"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "made by the agent") {
		t.Fatalf("the user's page = %d %q", w.Code, w.Body.String())
	}
	if w := get(bob, base+"index.html"); w.Code != http.StatusNotFound {
		t.Errorf("another user = %d, want 404", w.Code)
	}

	// A host service in a process of its own, as the core's API or a
	// database would be.
	other := freeTCPPort(t)
	svc := exec.Command("python3", "-c", `import socket,sys
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1", int(sys.argv[1]))); s.listen(8)
while True:
    c,_=s.accept(); print("accepted", flush=True); c.close()`, strconv.Itoa(other))
	out := &lockedBuf{}
	svc.Stdout = out
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Process.Kill(); _ = svc.Wait() })
	waitListening(t, other)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "accepted") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	probed := strings.Count(out.String(), "accepted")
	call(fmt.Sprintf(`{"op":"start","name":"web","command":"sleep 600","port":%d}`, other))
	foreign := expose(other)
	if w := get(alice, foreign+"index.html"); w.Code != http.StatusBadGateway {
		t.Errorf("a port only declared by a dummy server = %d %q, want 502", w.Code, w.Body.String())
	}
	if n := strings.Count(out.String(), "accepted"); n != probed {
		t.Errorf("the host service accepted %d connections through the route", n-probed)
	}
	// A socket to the same port is refused the same way (P9 PR 5): the
	// upgrade dials through the same tunnel and its listener check.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), *alice)))
	}))
	defer ts.Close()
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	if _, resp, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(ts.URL, "http")+foreign+"ws", &websocket.DialOptions{
		Subprotocols: []string{exposureSocketMarker, "bearer.lc_test"}}); err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a socket to a port only declared by a dummy server: %v %v, want 502", err, resp)
	}
	if n := strings.Count(out.String(), "accepted"); n != probed {
		t.Errorf("the host service accepted %d connections through a socket", n-probed)
	}

	// The run's release closes both pages.
	call(`{"op":"release","name":"web"}`)
	if w := get(alice, base+"index.html"); w.Code != http.StatusNotFound {
		t.Errorf("after release = %d, want 404", w.Code)
	}
}
