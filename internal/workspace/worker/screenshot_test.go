package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// shotEngine is a container engine as the screenshot op sees it: a server
// started with a command runs until killed; the browser (a program started
// by argv) prints shot, or, with hang, runs until killed.
type shotEngine struct {
	mu     sync.Mutex
	starts []engine.StartRequest
	shot   []byte
	hang   bool
	// gate, when set, holds the browser until it is closed.
	gate  chan struct{}
	kills int
}

func (e *shotEngine) Exec(context.Context, engine.ExecRequest) (engine.ExecResult, error) {
	return engine.ExecResult{}, nil
}

func (e *shotEngine) Start(_ context.Context, req engine.StartRequest) (engine.Process, error) {
	e.mu.Lock()
	e.starts = append(e.starts, req)
	e.mu.Unlock()
	p := &shotProc{e: e, done: make(chan struct{})}
	r, w := io.Pipe()
	p.r, p.w = r, w
	switch {
	case slices.Contains(req.Argv, updateRelaySource):
		// The update relay a container server gets says where it listens,
		// then runs until killed.
		go func() { _, _ = w.Write([]byte("P41000\n")) }()
	case !slices.Contains(req.Argv, browserScript) || e.hang:
		// A server, its update relay, or a browser that never finishes:
		// until Kill.
	default:
		go func() {
			if e.gate != nil {
				<-e.gate
			}
			_, _ = w.Write(e.shot)
			_ = w.Close()
			p.end()
		}()
	}
	return p, nil
}

func (e *shotEngine) Dial(context.Context, engine.DialRequest) (engine.Conn, error) {
	return nil, engine.ErrUnreachable
}
func (e *shotEngine) Forget(context.Context, string) error { return nil }
func (e *shotEngine) Close() error                         { return nil }

func (e *shotEngine) browserStarts() []engine.StartRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []engine.StartRequest
	for _, s := range e.starts {
		// A server's command, or the update relay a container server gets,
		// is not the browser.
		if slices.Contains(s.Argv, browserScript) {
			out = append(out, s)
		}
	}
	return out
}

type shotProc struct {
	e    *shotEngine
	r    *io.PipeReader
	w    *io.PipeWriter
	once sync.Once
	done chan struct{}
}

func (p *shotProc) end()                  { p.once.Do(func() { close(p.done) }) }
func (p *shotProc) Stdin() io.WriteCloser { return nopWriteCloser{} }
func (p *shotProc) Stdout() io.Reader     { return p.r }
func (p *shotProc) Interrupt() error      { return nil }
func (p *shotProc) Done() <-chan struct{} { return p.done }
func (p *shotProc) Stderr() string        { return "" }
func (p *shotProc) Kill() error {
	p.e.mu.Lock()
	p.e.kills++
	p.e.mu.Unlock()
	_ = p.w.CloseWithError(io.ErrClosedPipe)
	p.end()
	return nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(b []byte) (int, error) { return len(b), nil }
func (nopWriteCloser) Close() error                { return nil }

// A PNG's signature and a little of a body, enough to be read as one.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)

func shotWorker(t *testing.T, e *shotEngine) *worker {
	t.Helper()
	w, _ := newTestWorker(t)
	w.engine = e
	w.opt.Engine = workspace.EngineContainer
	w.opt.Labels = []string{workspace.LabelBrowser}
	t.Cleanup(w.servers.Close)
	return w
}

func shotRun(t *testing.T, w *worker, args workspace.ScreenshotArgs) answer {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws1", Op: workspace.OpScreenshot, Args: raw}, nil)
}

func serve8000(t *testing.T, w *worker) {
	t.Helper()
	startedServer(t, w, workspace.ServerStartArgs{Command: "python3 -m http.server 8000", Port: 8000})
}

func TestScreenshot_ReturnsTheBrowsersPicture(t *testing.T) {
	e := &shotEngine{shot: pngBytes}
	w := shotWorker(t, e)
	serve8000(t, w)
	a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/report", Width: 1280, Height: 800})
	if a.class != "" || !bytes.Equal(a.output, pngBytes) {
		t.Fatalf("screenshot = %s %s, %d bytes", a.class, a.detail, len(a.output))
	}
}

// The browser is told to load the session's own loopback and nothing else:
// every name but 127.0.0.1 resolves to nothing, no proxy, no WebRTC UDP,
// no QUIC, no background networking, and a profile of its own in the
// session's /tmp, gone after the shot, so no cookie or storage survives.
// Nothing of a credential is on its command line or in its environment.
func TestScreenshot_TheBrowserReachesOnlyTheSessionsLoopback(t *testing.T) {
	e := &shotEngine{shot: pngBytes}
	w := shotWorker(t, e)
	w.opt.Token = "worker-secret-token"
	serve8000(t, w)
	if a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600}); a.class != "" {
		t.Fatalf("screenshot: %s %s", a.class, a.detail)
	}
	starts := e.browserStarts()
	if len(starts) != 1 {
		t.Fatalf("browser starts = %d, want 1", len(starts))
	}
	argv := starts[0].Argv
	if len(starts[0].Env) != 0 {
		t.Errorf("the browser was given an environment: %q", starts[0].Env)
	}
	for _, flag := range []string{
		"--headless=new",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		"--no-proxy-server",
		"--disable-quic",
		"--disable-background-networking",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
	} {
		if !slices.Contains(argv, flag) {
			t.Errorf("the browser's command line lacks %q: %q", flag, argv)
		}
	}
	if !slices.Contains(argv, "http://127.0.0.1:8000/") || !slices.Contains(argv, "800,600") {
		t.Errorf("the browser does not load the loopback address at the viewport: %q", argv)
	}
	all := strings.Join(argv, " ")
	for _, bad := range []string{"worker-secret-token", "Authorization", "Bearer", "Cookie", "--proxy-server="} {
		if strings.Contains(all, bad) {
			t.Errorf("the browser's command line holds %q", bad)
		}
	}
	if !strings.Contains(all, "mktemp -d /tmp/") || !strings.Contains(all, `--user-data-dir="$d`) || !strings.Contains(all, `trap 'rm -rf "$d"' EXIT`) {
		t.Errorf("the browser's profile is not a fresh one in /tmp removed after the shot: %q", all)
	}
}

// Only the port of a running server of this workspace; only a path, never a
// URL; only a viewport within the bound. The worker checks again what the
// core checked.
func TestScreenshot_RefusesAnythingButItsOwnServersPath(t *testing.T) {
	e := &shotEngine{shot: pngBytes}
	w := shotWorker(t, e)
	serve8000(t, w)
	for name, args := range map[string]workspace.ScreenshotArgs{
		"external url":    {Port: 8000, Path: "http://evil.example.com/", Width: 800, Height: 600},
		"no slash":        {Port: 8000, Path: "evil.example.com/", Width: 800, Height: 600},
		"scheme-relative": {Port: 8000, Path: "//evil.example.com/", Width: 800, Height: 600},
		"wide":            {Port: 8000, Path: "/", Width: 1921, Height: 600},
		"tall":            {Port: 8000, Path: "/", Width: 800, Height: 1081},
	} {
		if a := shotRun(t, w, args); a.class != workspace.ClassInvalid {
			t.Errorf("%s: answer = %q %s, want %s", name, a.class, a.detail, workspace.ClassInvalid)
		}
	}
	if a := shotRun(t, w, workspace.ScreenshotArgs{Port: 9000, Path: "/", Width: 800, Height: 600}); a.class != workspace.ClassPortNotServed {
		t.Errorf("a port no server declared: answer = %q %s", a.class, a.detail)
	}
	if n := len(e.browserStarts()); n != 0 {
		t.Errorf("a refused shot started the browser %d times", n)
	}
}

// A worker without the browser label, and the host engine, offer no
// screenshot, whatever the core asked.
func TestScreenshot_RefusedWithoutTheBrowserOrOnTheHostEngine(t *testing.T) {
	e := &shotEngine{shot: pngBytes}
	w := shotWorker(t, e)
	serve8000(t, w)
	w.opt.Labels = nil
	if a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600}); a.class != workspace.ClassBrowserUnavailable {
		t.Errorf("no browser label: answer = %q %s", a.class, a.detail)
	}
	w.opt.Labels = []string{workspace.LabelBrowser}
	w.opt.Engine = workspace.EngineHost
	if a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600}); a.class != workspace.ClassBrowserUnavailable {
		t.Errorf("host engine: answer = %q %s", a.class, a.detail)
	}
	if n := len(e.browserStarts()); n != 0 {
		t.Errorf("a refused shot started the browser %d times", n)
	}
}

// A shot that does not finish in time is killed and refused.
func TestScreenshot_KilledPastItsTime(t *testing.T) {
	e := &shotEngine{hang: true}
	w := shotWorker(t, e)
	w.shotTimeout = 200 * time.Millisecond
	serve8000(t, w)
	start := time.Now()
	a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600})
	if a.class != workspace.ClassBrowserTimedOut {
		t.Fatalf("answer = %q %s, want %s", a.class, a.detail, workspace.ClassBrowserTimedOut)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the timed-out shot took %v", took)
	}
	e.mu.Lock()
	kills := e.kills
	e.mu.Unlock()
	if kills == 0 {
		t.Error("the browser was not killed")
	}
}

// What the browser prints is read only up to one byte past the bound, so a
// page cannot make the worker hold more; the core then refuses it whole.
func TestScreenshot_ReadsAtMostOneBytePastTheBound(t *testing.T) {
	e := &shotEngine{shot: append(append([]byte(nil), pngBytes...), bytes.Repeat([]byte{1}, workspace.MaxScreenshotBytes)...)}
	w := shotWorker(t, e)
	serve8000(t, w)
	a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600})
	if a.class != "" || len(a.output) != workspace.MaxScreenshotBytes+1 {
		t.Fatalf("answer = %q %s, %d bytes; want the bound plus one", a.class, a.detail, len(a.output))
	}
}

// What is not a PNG is no picture.
func TestScreenshot_RefusesWhatIsNotAPicture(t *testing.T) {
	for name, out := range map[string][]byte{"empty": nil, "text": []byte("<html>not a picture</html>")} {
		e := &shotEngine{shot: out}
		w := shotWorker(t, e)
		serve8000(t, w)
		if a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600}); a.class != workspace.ClassFailed {
			t.Errorf("%s: answer = %q %s", name, a.class, a.detail)
		}
	}
}

// One browser per workspace at a time: a second shot while one is taken is
// refused, and the next after it ends goes through.
func TestScreenshot_OneBrowserPerWorkspaceAtATime(t *testing.T) {
	e := &shotEngine{shot: pngBytes, gate: make(chan struct{})}
	w := shotWorker(t, e)
	serve8000(t, w)
	first := make(chan answer, 1)
	go func() {
		first <- shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.browserStarts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600}); a.class != workspace.ClassBrowserBusy {
		t.Errorf("a second shot at once: answer = %q %s", a.class, a.detail)
	}
	close(e.gate)
	if a := <-first; a.class != "" {
		t.Fatalf("first shot: %s %s", a.class, a.detail)
	}
	if a := shotRun(t, w, workspace.ScreenshotArgs{Port: 8000, Path: "/", Width: 800, Height: 600}); a.class != "" {
		t.Errorf("the shot after: %s %s", a.class, a.detail)
	}
}
