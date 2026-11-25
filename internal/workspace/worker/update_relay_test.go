package worker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// The container engine's update relay (D-P9-66), run here as a host
// process: what it accepts, what it refuses, and the bounds, the same as
// the host listener's. The session-only address is the docker test's.
func relayWorker(t *testing.T) *worker {
	t.Helper()
	requirePython(t)
	w, _ := newTestWorker(t)
	w.opt.Engine = workspace.EngineContainer
	t.Cleanup(w.servers.Close)
	return w
}

func TestUpdateRelay_QueuesPostsAndRefusesTheRest(t *testing.T) {
	w := relayWorker(t)
	u, err := w.updateURL(context.Background(), "ws1", filepath.Join(w.opt.Root, "ws1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.HasSuffix(u, "/") {
		t.Fatalf("relay url = %q", u)
	}
	base := strings.TrimSuffix(u, "/")
	for _, p := range []string{"/x", "/a/b", "/x/"} {
		if code, body := post(t, base+p, []byte("no")); code != http.StatusNotFound || body != "" {
			t.Errorf("post to %s = %d %q, want 404 with no body", p, code, body)
		}
	}
	if code, _ := post(t, u, bytes.Repeat([]byte("x"), workspace.MaxUpdateBytes+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the size bound = %d, want 413", code)
	}
	for i := range workspace.MaxUpdates {
		if code, _ := post(t, u, []byte(fmt.Sprintf("u%d", i))); code != http.StatusAccepted {
			t.Fatalf("post %d = %d", i, code)
		}
	}
	if code, _ := post(t, u, []byte("over")); code != http.StatusTooManyRequests {
		t.Errorf("past the count bound = %d, want 429", code)
	}
	a := awaitUpdate(t, w, "ws1", time.Second)
	if updateText(a) != "u0" || a.Pending != workspace.MaxUpdates-1 || a.Dropped != 1 {
		t.Errorf("await = %+v %q", a, updateText(a))
	}
}

// A relay that ended (a restore stops the session) is made again at the
// next server start.
func TestUpdateRelay_AnEndedRelayIsMadeAgain(t *testing.T) {
	w := relayWorker(t)
	real := filepath.Join(w.opt.Root, "ws1")
	u1, err := w.updateURL(context.Background(), "ws1", real)
	if err != nil {
		t.Fatal(err)
	}
	b := w.servers.inboxFor("ws1")
	b.feedMu.Lock()
	b.stop()
	done := b.done
	b.feedMu.Unlock()
	<-done
	u2, err := w.updateURL(context.Background(), "ws1", real)
	if err != nil || u2 == u1 {
		t.Fatalf("after the relay ended: %q, %v; want a new address", u2, err)
	}
	if code, _ := post(t, u2, []byte("again")); code != http.StatusAccepted {
		t.Errorf("post to the new relay = %d", code)
	}
}

// relayImageEnv names a session image with python3 (as the engine's relay
// test does).
const relayImageEnv = "LOOMCYCLE_TEST_RELAY_IMAGE"

// In a container session the address is the session's own loopback: a
// server in the session posts to it and await takes the update, while the
// same address from the worker's host reaches nothing.
func TestUpdateRelay_ContainerAddressIsTheSessionsAlone(t *testing.T) {
	// workspacetest.RequireDocker imports this package; its check, inline.
	skip := t.Skipf
	if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
		skip = t.Fatalf
	}
	cli, err := exec.LookPath("docker")
	if err != nil {
		skip("no docker on PATH: %v", err)
		return
	}
	if out, err := exec.Command(cli, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		skip("the container daemon does not answer: %v: %s", err, out)
		return
	}
	image := os.Getenv(relayImageEnv)
	if image == "" {
		if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
			t.Fatalf("%s is not set: build Dockerfile.workspace and name it", relayImageEnv)
		}
		t.Skipf("%s is not set", relayImageEnv)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "ws1"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := engine.NewContainer(engine.ContainerOptions{
		CLI: cli, Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), WorkerKey: engine.WorkerKey(t.Name(), root),
		MemoryMB: 256, Pids: 128, TmpMB: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	w := &worker{opt: Options{Root: root, Capacity: 4, Engine: workspace.EngineContainer}, engine: c}
	t.Cleanup(w.servers.Close)
	src := `import os, urllib.request, time
u = os.environ["` + workspace.UpdateURLEnv + `"]
urllib.request.urlopen(urllib.request.Request(u, data=b"from the session", method="POST"))
print("posted", flush=True)
time.sleep(600)`
	if err := os.WriteFile(filepath.Join(root, "ws1", "poster.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if a := serverOpRun(t, w, workspace.OpServerStart, workspace.ServerStartArgs{Command: "python3 poster.py"}); a.class != "" {
		t.Fatalf("start: %s %s", a.class, a.detail)
	}
	if a := awaitUpdate(t, w, "ws1", 60*time.Second); updateText(a) != "from the session" {
		t.Fatalf("await = %+v", a)
	}
	u, err := w.updateURL(context.Background(), "ws1", filepath.Join(root, "ws1"))
	if err != nil {
		t.Fatal(err)
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	if resp, err := cl.Post(u, "text/plain", strings.NewReader("from the host")); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted {
			t.Errorf("the worker's host reached the session's update address (%d)", resp.StatusCode)
		}
	}
	if a := awaitUpdate(t, w, "ws1", 500*time.Millisecond); !a.TimedOut {
		t.Errorf("a post from the host was queued: %+v", a)
	}
}
