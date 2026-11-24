package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// relayImageEnv names a session image with python3, which the tunnel's
// relay needs (busybox, the other container tests' image, has none). CI's
// workspace-container job sets it to the image built from
// Dockerfile.workspace, with LOOMCYCLE_REQUIRE_DOCKER, so a skip there is a
// failure.
const relayImageEnv = "LOOMCYCLE_TEST_RELAY_IMAGE"

// The relay a tunnel uses in a container session (P9 plan §3.2, D-P9-7)
// reaches the session's own loopback and nothing else: a port that listens
// on the worker's host loopback is unreachable from it, the same port
// served inside the session is reached, and the session's network is still
// none (the relay gives it none).
func TestContainerRelay_ReachesOnlyTheSessionsOwnLoopback(t *testing.T) {
	cli, _ := workspacetest.RequireDocker(t)
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
	key := engine.WorkerKey(t.Name(), root)
	c, err := engine.NewContainer(engine.ContainerOptions{
		CLI: cli, Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), WorkerKey: key,
		MemoryMB: 256, Pids: 128, TmpMB: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	dir := filepath.Join(root, "ws1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// A listener on the worker's own loopback, as another workspace's or
	// the core's port would be.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if conn, err := c.Dial(ctx, engine.DialRequest{Workspace: "ws1", Root: dir, Port: port}); !errors.Is(err, engine.ErrUnreachable) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("a port only the worker's host listens on = %v, want ErrUnreachable", err)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the worker's host listener accepted %d connections from the session", n)
	}

	p, err := c.Start(ctx, engine.StartRequest{Workspace: "ws1", Root: dir, Dir: dir,
		Command: fmt.Sprintf("python3 -m http.server %d --bind 127.0.0.1", port)})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	deadline := time.Now().Add(60 * time.Second)
	var answer string
	for answer == "" {
		conn, err := c.Dial(ctx, engine.DialRequest{Workspace: "ws1", Root: dir, Port: port})
		if err == nil {
			_, _ = io.WriteString(conn, "GET / HTTP/1.0\r\n\r\n")
			b, _ := io.ReadAll(conn)
			_ = conn.Close()
			answer = string(b)
			break
		}
		if !errors.Is(err, engine.ErrUnreachable) || time.Now().After(deadline) {
			t.Fatalf("dial the session's own server: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !strings.HasPrefix(answer, "HTTP/1.0 200") {
		t.Errorf("the session's server answered %.200q", answer)
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("the worker's host listener accepted %d connections", n)
	}
	out, err := exec.Command(cli, "inspect", "--format", "{{.HostConfig.NetworkMode}}", "loomws-"+key+"-ws1").Output()
	if err != nil {
		t.Fatal(err)
	}
	if mode := strings.TrimSpace(string(out)); mode != "none" {
		t.Errorf("the session's network after a relay = %q, want none", mode)
	}
}
