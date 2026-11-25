package engine

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hostEcho answers each request with the workspace it was served for and the
// path it asked.
func hostEcho(ws string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, ws+" "+r.RequestURI)
	})
}

// envOf runs `env` in workspace ws and returns what it printed, by name.
func envOf(t *testing.T, h *Host, ws string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	res, err := h.Exec(context.Background(), ExecRequest{Workspace: ws, Root: dir, Dir: dir, Command: "env", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(res.Output), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func get(t *testing.T, u string) (string, error) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// Each workspace's commands reach its routes at a loopback port of its own,
// named in their environment with the package managers pointed at it; a
// request there is served for that workspace alone, and the port stays the
// same across its commands.
func TestHost_EachWorkspaceReachesItsRoutesAtALoopbackPortOfItsOwn(t *testing.T) {
	h := &Host{Egress: hostEcho}
	t.Cleanup(func() { _ = h.Close() })
	e1, e2 := envOf(t, h, "w1"), envOf(t, h, "w2")
	base := e1["LOOMCYCLE_EGRESS"]
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || !strings.HasSuffix(u.Path, "/") {
		t.Fatalf("LOOMCYCLE_EGRESS = %q, want http://127.0.0.1:<port>/<secret>/", base)
	}
	want := map[string]string{
		"PIP_INDEX_URL":       base + "pypi/simple/",
		"PIP_TRUSTED_HOST":    "127.0.0.1",
		"npm_config_registry": base + "npm/",
		"npm_config_audit":    "false",
	}
	for k, v := range want {
		if e1[k] != v {
			t.Errorf("%s = %q, want %q", k, e1[k], v)
		}
	}
	if e2["LOOMCYCLE_EGRESS"] == base {
		t.Fatal("two workspaces share one egress port")
	}
	for ws, b := range map[string]string{"w1": base, "w2": e2["LOOMCYCLE_EGRESS"]} {
		body, err := get(t, b+"pypi/simple/numpy/")
		if err != nil || body != ws+" /pypi/simple/numpy/" {
			t.Errorf("%s's port answered %q, %v", ws, body, err)
		}
	}
	if again := envOf(t, h, "w1")["LOOMCYCLE_EGRESS"]; again != base {
		t.Errorf("w1's second command got %q, want the same port %q", again, base)
	}
}

// A program Start starts (a kernel, a server) gets the same environment.
func TestHost_AStartedProgramGetsTheWorkspacesEgressEnvironment(t *testing.T) {
	h := &Host{Egress: hostEcho}
	t.Cleanup(func() { _ = h.Close() })
	base := envOf(t, h, "w1")["LOOMCYCLE_EGRESS"]
	dir := t.TempDir()
	p, err := h.Start(context.Background(), StartRequest{Workspace: "w1", Root: dir, Dir: dir, Command: "env"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p.Stdout())
	<-p.Done()
	if !strings.Contains(string(out), "LOOMCYCLE_EGRESS="+base+"\n") {
		t.Fatalf("the started program's environment = %q, want LOOMCYCLE_EGRESS=%s", out, base)
	}
}

// Forget closes the workspace's port; Close closes every one.
func TestHost_ForgetAndCloseEndTheEgressPorts(t *testing.T) {
	h := &Host{Egress: hostEcho}
	b1, b2 := envOf(t, h, "w1")["LOOMCYCLE_EGRESS"], envOf(t, h, "w2")["LOOMCYCLE_EGRESS"]
	if err := h.Forget(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, b1+"x/"); err == nil {
		t.Error("a forgotten workspace's port still answers")
	}
	if _, err := get(t, b2+"x/"); err != nil {
		t.Errorf("another workspace's port stopped: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, b2+"x/"); err == nil {
		t.Error("a port still answers after Close")
	}
}

// Without Egress a command gets no egress environment and no port is opened.
func TestHost_WithoutEgressACommandGetsNoEgressEnvironment(t *testing.T) {
	e := envOf(t, &Host{}, "w1")
	for _, k := range []string{"LOOMCYCLE_EGRESS", "PIP_INDEX_URL", "npm_config_registry"} {
		if v, ok := e[k]; ok {
			t.Errorf("%s = %q, want unset", k, v)
		}
	}
}

// countingEcho is hostEcho that counts the requests that reached it: what
// would have gone on to the core.
func countingEcho(n *int32) func(string) http.Handler {
	return func(ws string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(n, 1)
			_, _ = io.WriteString(w, ws+" "+r.RequestURI)
		})
	}
}

// rawGet sends a GET to addr with the given Host header and request target.
func rawGet(t *testing.T, addr, host, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The port's address carries a secret of the workspace's own after the
// port: a request without it, or with another, is refused before anything
// reaches the core, and the refusal does not echo it. A browser page that a
// rebound name points at the port, or another process that guesses the
// port, holds no secret.
func TestHost_AnEgressRequestWithoutTheWorkspacesSecretIsRefused(t *testing.T) {
	var n int32
	h := &Host{Egress: countingEcho(&n)}
	t.Cleanup(func() { _ = h.Close() })
	base := envOf(t, h, "w1")["LOOMCYCLE_EGRESS"]
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Trim(u.Path, "/")
	if len(secret) < 32 || strings.Contains(secret, "/") {
		t.Fatalf("LOOMCYCLE_EGRESS = %q, want http://127.0.0.1:<port>/<secret>/ with a long secret", base)
	}
	for _, target := range []string{"/pypi/simple/", "/" + strings.Repeat("a", len(secret)) + "/pypi/simple/", "/" + secret[:len(secret)-1] + "/pypi/simple/", "/" + secret, "/" + secret + "x/pypi/"} {
		code, body := rawGet(t, u.Host, u.Host, target)
		if code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", target, code)
		}
		if strings.Contains(body, secret) {
			t.Errorf("%s: the refusal echoes the secret", target)
		}
	}
	if got := atomic.LoadInt32(&n); got != 0 {
		t.Fatalf("%d requests without the secret reached the core", got)
	}
	code, body := rawGet(t, u.Host, u.Host, "/"+secret+"/pypi/simple/six/")
	if code != http.StatusOK || body != "w1 /pypi/simple/six/" {
		t.Fatalf("with the secret = %d %q, want the request passed on without it", code, body)
	}
}

// A request whose Host is not the port's own address is refused: a page a
// rebound DNS name sent here names its own host.
func TestHost_AnEgressRequestForAnotherHostIsRefused(t *testing.T) {
	var n int32
	h := &Host{Egress: countingEcho(&n)}
	t.Cleanup(func() { _ = h.Close() })
	u, err := url.Parse(envOf(t, h, "w1")["LOOMCYCLE_EGRESS"])
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(u.Host)
	for _, host := range []string{"evil.example", "evil.example:" + port, "localhost:" + port, "127.0.0.1", "127.0.0.1:1"} {
		if code, _ := rawGet(t, u.Host, host, u.Path+"pypi/simple/"); code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, code)
		}
	}
	if got := atomic.LoadInt32(&n); got != 0 {
		t.Fatalf("%d requests for another host reached the core", got)
	}
}

// Every workspace's secret is its own, and a port opened again gets a new
// one.
func TestHost_EachEgressPortHasASecretOfItsOwn(t *testing.T) {
	h := &Host{Egress: hostEcho}
	t.Cleanup(func() { _ = h.Close() })
	seen := map[string]bool{}
	for _, ws := range []string{"w1", "w2", "w3"} {
		u, _ := url.Parse(envOf(t, h, ws)["LOOMCYCLE_EGRESS"])
		seen[u.Path] = true
	}
	_ = h.Forget(context.Background(), "w1")
	u, _ := url.Parse(envOf(t, h, "w1")["LOOMCYCLE_EGRESS"])
	seen[u.Path] = true
	if len(seen) != 4 {
		t.Fatalf("%d distinct secrets for 4 ports", len(seen))
	}
}
