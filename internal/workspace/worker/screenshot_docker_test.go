package worker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// browserImageEnv names the browser session image (Dockerfile.workspace,
// --target browser).
const browserImageEnv = "LOOMCYCLE_TEST_BROWSER_IMAGE"

// shotServer serves on argv[1] a page that asks for pictures four ways:
// by 127.0.0.1 at its own port, by a host name meaning the loopback, at a
// second loopback port (argv[2]: another server of the same workspace), and
// at a literal address outside the session. It logs every path it is asked
// for, so the test sees which requests the browser could make.
const shotServer = `import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port, other = int(sys.argv[1]), int(sys.argv[2])
page = ("<html><body style='background:#36c'><h1>hello</h1>"
        "<img src='http://127.0.0.1:%d/by-address.png'>"
        "<img src='http://localhost:%d/by-name.png'>"
        "<img src='http://127.0.0.1:%d/other-port.png'>"
        "<img src='http://10.0.0.1:%d/literal.png'></body></html>" % (port, port, other, port)).encode()
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        print("got", self.path, flush=True)
        body = page if self.path == "/" else b""
        self.send_response(200 if self.path == "/" else 404)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", port), H).serve_forever()
`

// A real shot in a real session of the browser image: the picture is a
// PNG, the page's own server is reached by its address, and a host name,
// even one meaning the loopback, resolves to nothing, so a page cannot name
// anything outside the session. The worker finds the browser in the image
// at its start, as the label says.
func TestContainerScreenshot_ReachesOnlyTheLoopbackAddress(t *testing.T) {
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
	image := os.Getenv(browserImageEnv)
	if image == "" {
		if os.Getenv("LOOMCYCLE_REQUIRE_DOCKER") == "1" {
			t.Fatalf("%s is not set: build Dockerfile.workspace --target browser and name it", browserImageEnv)
		}
		t.Skipf("%s is not set", browserImageEnv)
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
		MemoryMB: 1024, Pids: 256, TmpMB: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if ok, err := c.ProbeBrowser(ctx); err != nil || !ok {
		t.Fatalf("ProbeBrowser = %v, %v; want the browser image to carry one", ok, err)
	}
	w := &worker{opt: Options{Root: root, Capacity: 4, Engine: workspace.EngineContainer, Labels: []string{workspace.LabelBrowser}}, engine: c}
	t.Cleanup(w.servers.Close)
	if err := os.WriteFile(filepath.Join(root, "ws1", "page.py"), []byte(shotServer), 0o644); err != nil {
		t.Fatal(err)
	}
	const port, other = 8765, 8766
	info := startedServer(t, w, workspace.ServerStartArgs{Command: fmt.Sprintf("python3 page.py %d %d", port, other), Port: port})
	second := startedServer(t, w, workspace.ServerStartArgs{Command: fmt.Sprintf("python3 page.py %d %d", other, port), Port: other})
	// Both are up once each answers in the session.
	for _, p := range []int{port, other} {
		deadline := time.Now().Add(60 * time.Second)
		for {
			res, err := c.Exec(ctx, engine.ExecRequest{Workspace: "ws1", Root: filepath.Join(root, "ws1"), Dir: filepath.Join(root, "ws1"),
				Command: fmt.Sprintf(`python3 -c "import socket; socket.create_connection(('127.0.0.1', %d), 2)"`, p), Timeout: 10 * time.Second})
			if err == nil && res.ExitCode == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	a := shotRun(t, w, workspace.ScreenshotArgs{Port: port, Path: "/", Width: 800, Height: 600})
	if a.class != "" {
		t.Fatalf("screenshot: %s %s", a.class, a.detail)
	}
	if !bytes.HasPrefix(a.output, pngSignature) {
		t.Fatalf("the picture is not a PNG (%d bytes)", len(a.output))
	}
	logOf := func(id string) string {
		a := serverOpRun(t, w, workspace.OpServerLogs, workspace.ServerLogsArgs{Server: id})
		return string(a.output)
	}
	log, otherLog := logOf(info.Server), logOf(second.Server)
	if !strings.Contains(log, "/by-address.png") {
		t.Errorf("the picture at the declared port was not asked for: %s", log)
	}
	if strings.Contains(log, "/by-name.png") {
		t.Errorf("a host name reached the server, so the browser resolves names: %s", log)
	}
	if strings.Contains(otherLog, "/other-port.png") {
		t.Errorf("a second loopback port was reached, so the browser is not confined to the declared one: %s", otherLog)
	}
}
