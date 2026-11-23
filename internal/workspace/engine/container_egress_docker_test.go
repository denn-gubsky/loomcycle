package engine_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// The tests below run real sessions with egress (RFC GW §4.3, §6.3, plan O12):
// each session on a network of its own with no route out, shared only with a
// gateway that answers as "egress" and forwards to the worker over a Unix
// socket of that workspace alone. They need an image with python3 (the
// gateway is a small Python forwarder): LOOMCYCLE_TEST_EGRESS_IMAGE, which CI
// sets to the session image built from Dockerfile.workspace.

// egressCanary is put in the worker's own environment; nothing a session or
// a gateway can read may hold it.
const egressCanary = "egress-canary-7f3a1c0de-never-in-a-session"

type egressSession struct {
	*session
	dir  string
	mu   sync.Mutex
	seen []string // "<workspace> <request URI>" the worker's handler answered
}

func newEgressSession(t *testing.T) *egressSession {
	t.Helper()
	cli, _ := workspacetest.RequireDocker(t)
	image := os.Getenv("LOOMCYCLE_TEST_EGRESS_IMAGE")
	if image == "" {
		if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
			t.Fatal("LOOMCYCLE_TEST_EGRESS_IMAGE is not set: the egress tests need an image with python3")
		}
		t.Skip("LOOMCYCLE_TEST_EGRESS_IMAGE is not set: the egress tests need an image with python3")
	}
	requireUnixSocketMounts(t, cli, image)
	t.Setenv("LOOMCYCLE_TEST_EGRESS_CANARY", egressCanary)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A short directory: a Unix socket's path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "lce")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	es := &egressSession{dir: dir}
	key := engine.WorkerKey(t.Name(), root)
	c, err := engine.NewContainer(engine.ContainerOptions{
		CLI: cli, Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), WorkerKey: key,
		MemoryMB: 256, Pids: 128, TmpMB: 16,
		Egress: &engine.ContainerEgress{
			Dir: dir,
			Handler: func(workspace string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					es.mu.Lock()
					es.seen = append(es.seen, workspace+" "+r.RequestURI)
					es.mu.Unlock()
					_, _ = io.WriteString(w, "answered-for-"+workspace)
				})
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	es.session = &session{t: t, e: c, cli: cli, root: root, key: key}
	return es
}

// requireUnixSocketMounts checks that a container can connect to a Unix
// socket the host bind-mounts into it. Docker Desktop on macOS cannot (the
// socket does not cross into its VM), so there the tests skip, and fail when
// a daemon is required.
func requireUnixSocketMounts(t *testing.T, cli, image string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "lcs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	l, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		if c, err := l.Accept(); err == nil {
			_, _ = io.WriteString(c, "ok")
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// #nosec G204 -- a test: the CLI and image are the test's own
	out, err := exec.CommandContext(ctx, cli, "run", "--rm", "--network", "none", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", dir+":/s:ro", image, "python3", "-c",
		"import socket; u=socket.socket(socket.AF_UNIX); u.connect('/s/s.sock'); print(u.recv(8).decode())").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		msg := fmt.Sprintf("this daemon cannot connect a container to a bind-mounted Unix socket (Docker Desktop on macOS): %v: %s", err, out)
		if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

// fetch is a session's GET of url, reported as "ok <body>" or "err <time>".
const fetchPy = `
import sys, time, urllib.request
t = time.monotonic()
try:
    print("ok " + urllib.request.urlopen(sys.argv[1], timeout=5).read().decode())
except Exception as e:
    print("err %.2f %s" % (time.monotonic() - t, type(e).__name__))
`

// connectPy tries a TCP connection to host:port and reports "open" or
// "closed <seconds>".
const connectPy = `
import socket, sys, time
t = time.monotonic()
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=3).close()
    print("open")
except Exception as e:
    print("closed %.2f %s" % (time.monotonic() - t, type(e).__name__))
`

func (s *egressSession) run(id, script string, args ...string) string {
	s.t.Helper()
	cmd := "python3 -I -c " + shellQuote(script)
	for _, a := range args {
		cmd += " " + shellQuote(a)
	}
	res := s.exec(id, cmd, 120*time.Second)
	return strings.TrimSpace(string(res.Output))
}

func (s *egressSession) inspect(format, name string) string {
	s.t.Helper()
	// #nosec G204 -- a test: the CLI is the test's own
	out, err := exec.Command(s.cli, "inspect", "--format", format, name).CombinedOutput()
	if err != nil {
		s.t.Fatalf("inspect %s: %v: %s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s *egressSession) name(id string) string { return "loomws-" + s.key + "-" + id }

// A session reaches the worker through http://egress/, and the worker knows
// which workspace asked from the socket the request came in on, not from
// anything in the request.
func TestContainerEgress_ASessionReachesTheWorkerThroughEgressAsItself(t *testing.T) {
	s := newEgressSession(t)
	s.workspace("ws1")
	s.workspace("ws2")
	if got := s.run("ws1", fetchPy, "http://egress/pypi/simple/numpy/?x=1"); got != "ok answered-for-ws1" {
		t.Fatalf("ws1 got %q", got)
	}
	if got := s.run("ws2", fetchPy, "http://egress/npm/left-pad"); got != "ok answered-for-ws2" {
		t.Fatalf("ws2 got %q", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := []string{"ws1 /pypi/simple/numpy/?x=1", "ws2 /npm/left-pad"}
	if strings.Join(s.seen, "|") != strings.Join(want, "|") {
		t.Fatalf("the worker saw %q, want %q", s.seen, want)
	}
	env := s.run("ws1", `import os; print(os.environ.get("PIP_INDEX_URL",""), os.environ.get("npm_config_registry",""), os.environ.get("LOOMCYCLE_EGRESS",""))`)
	if env != "http://egress/pypi/simple/ http://egress/npm/ http://egress/" {
		t.Errorf("the session's package managers are set up as %q", env)
	}
}

// A session has no route out: only its own network's subnet, no default
// route; a name other than egress fails to resolve, and a public address
// fails to connect, each in under a second.
func TestContainerEgress_ASessionHasNoRouteOutButEgress(t *testing.T) {
	s := newEgressSession(t)
	s.workspace("ws1")
	routes := s.run("ws1", `print(open("/proc/net/route").read())`)
	for _, line := range strings.Split(routes, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "00000000" {
			t.Errorf("the session has a default route: %q", line)
		}
	}
	resolve := `
import socket, sys, time
t = time.monotonic()
try:
    socket.getaddrinfo(sys.argv[1], 80)
    print("resolved")
except Exception:
    print("failed %.2f" % (time.monotonic() - t))
`
	for _, host := range []string{"example.com", "pypi.org", "host.docker.internal", "localhost.localdomain.example"} {
		got := s.run("ws1", resolve, host)
		if !strings.HasPrefix(got, "failed ") {
			t.Errorf("%s: %q, want a failed lookup", host, got)
			continue
		}
		if sec, _ := strconv.ParseFloat(strings.TrimPrefix(got, "failed "), 64); sec >= 1 {
			t.Errorf("%s: the lookup took %.2fs, want under a second", host, sec)
		}
	}
	if got := s.run("ws1", resolve, "egress"); got != "resolved" {
		t.Errorf("egress: %q, want resolved", got)
	}
	for _, addr := range [][2]string{{"1.1.1.1", "80"}, {"8.8.8.8", "53"}, {"169.254.169.254", "80"}} {
		got := s.run("ws1", connectPy, addr[0], addr[1])
		if !strings.HasPrefix(got, "closed ") {
			t.Errorf("%s:%s: %q, want closed", addr[0], addr[1], got)
			continue
		}
		if sec, _ := strconv.ParseFloat(strings.Fields(got)[1], 64); sec >= 1 {
			t.Errorf("%s:%s: took %.2fs to fail, want under a second", addr[0], addr[1], sec)
		}
	}
}

// One session cannot reach another workspace's session, nor its gateway.
func TestContainerEgress_ASessionCannotReachAnotherWorkspace(t *testing.T) {
	s := newEgressSession(t)
	s.workspace("ws1")
	s.workspace("ws2")
	// A server in ws2's session, on every address.
	s.exec("ws2", "nohup python3 -I -m http.server 8000 >/dev/null 2>&1 &", 30*time.Second)
	if got := s.run("ws2", fetchPy, "http://127.0.0.1:8000/"); !strings.HasPrefix(got, "ok") {
		t.Fatalf("the control failed: ws2 cannot reach its own server: %q", got)
	}
	_ = s.run("ws1", fetchPy, "http://egress/x") // starts ws1's session and gateway
	ws2IP := s.inspect("{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", s.name("ws2"))
	gw2IP := s.inspect("{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", s.name("ws2")+"-gw")
	for _, ip := range strings.Fields(ws2IP + " " + gw2IP) {
		for _, port := range []string{"8000", "80"} {
			if got := s.run("ws1", connectPy, ip, port); !strings.HasPrefix(got, "closed") {
				t.Errorf("ws1 reached %s:%s of ws2: %q", ip, port, got)
			}
		}
	}
	// The gateway mounts only its own workspace's socket directory.
	mounts := s.inspect("{{range .Mounts}}{{.Source}} {{end}}", s.name("ws1")+"-gw")
	if strings.Contains(mounts, s.root) {
		t.Errorf("ws1's gateway mounts the worker's root: %s", mounts)
	}
	sessionMounts := s.inspect("{{range .Mounts}}{{.Source}} {{end}}", s.name("ws1"))
	if strings.Contains(sessionMounts, s.dir) {
		t.Errorf("ws1's session mounts the egress directory: %s", sessionMounts)
	}
}

// A session cannot reach the worker or the core directly: a server on every
// address of this host, which a session on the default bridge would reach
// through the bridge's gateway, is closed to it.
func TestContainerEgress_ASessionCannotReachTheWorkerOrTheHost(t *testing.T) {
	s := newEgressSession(t)
	s.workspace("ws1")
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = s.run("ws1", fetchPy, "http://egress/x")
	netName := s.inspect("{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", s.name("ws1"))
	targets := []string{"172.17.0.1", "host.docker.internal"}
	// #nosec G204 -- a test: the CLI is the test's own
	if out, err := exec.Command(s.cli, "network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}", strings.TrimSpace(netName)).CombinedOutput(); err == nil {
		targets = append(targets, strings.Fields(string(out))...)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				targets = append(targets, ipn.IP.String())
			}
		}
	}
	for _, host := range targets {
		if got := s.run("ws1", connectPy, host, port); !strings.HasPrefix(got, "closed") {
			t.Errorf("the session reached this host's port %s at %s: %q", port, host, got)
		}
	}
}

// Nothing a session or a gateway can read holds the worker's own environment.
func TestContainerEgress_TheWorkersSecretsReachNeitherSessionNorGateway(t *testing.T) {
	s := newEgressSession(t)
	s.workspace("ws1")
	_ = s.run("ws1", fetchPy, "http://egress/x")
	if got := s.run("ws1", `import os, glob
out = []
for p in ["/proc/self/environ"] + glob.glob("/proc/*/environ"):
    try: out.append(open(p, "rb").read().decode(errors="replace"))
    except Exception: pass
print("\n".join(out))`); strings.Contains(got, egressCanary) {
		t.Error("the session can read the worker's environment")
	}
	for _, name := range []string{s.name("ws1"), s.name("ws1") + "-gw"} {
		if env := s.inspect("{{json .Config.Env}} {{json .Config.Cmd}}", name); strings.Contains(env, egressCanary) {
			t.Errorf("%s was started with the worker's environment: %s", name, env)
		}
	}
	files := s.run("ws1", `import os
for d in ["/workspace", "/tmp", "/run"]:
    for r, _, fs in os.walk(d):
        for f in fs:
            try: print(open(os.path.join(r, f), "rb").read().decode(errors="replace"))
            except Exception: pass`)
	if strings.Contains(files, egressCanary) {
		t.Error("a file the session can read holds the worker's environment")
	}
}

// Forget removes the session, its gateway, its network and its socket.
func TestContainerEgress_ForgetRemovesTheGatewayNetworkAndSocket(t *testing.T) {
	s := newEgressSession(t)
	s.workspace("ws1")
	_ = s.run("ws1", fetchPy, "http://egress/x")
	netName := strings.TrimSpace(s.inspect("{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", s.name("ws1")))
	if err := s.e.Forget(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{s.name("ws1"), s.name("ws1") + "-gw"} {
		// #nosec G204 -- a test: the CLI is the test's own
		if exec.Command(s.cli, "inspect", name).Run() == nil {
			t.Errorf("%s is still there", name)
		}
	}
	// #nosec G204 -- a test: the CLI is the test's own
	if exec.Command(s.cli, "network", "inspect", netName).Run() == nil {
		t.Errorf("network %s is still there", netName)
	}
	left, _ := filepath.Glob(filepath.Join(s.dir, "*", "*", "*.sock"))
	if len(left) > 0 {
		t.Errorf("sockets left: %v", left)
	}
}
