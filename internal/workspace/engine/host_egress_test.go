package engine

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/" {
		t.Fatalf("LOOMCYCLE_EGRESS = %q, want http://127.0.0.1:<port>/", base)
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
