package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// browserFlags are the browser's switches. The page is the workspace's own,
// model-written, so each one closes a way out of the session's loopback:
//
//   - every request goes to a proxy that is not there (browserProxy) but
//     the declared port on 127.0.0.1, the one entry of its bypass list, so
//     a page cannot reach the egress gateway, another loopback port or
//     anything else by address (P10 review, MEDIUM); "<-loopback>" drops
//     Chromium's own bypass of loopback, without which every loopback port
//     goes direct. A PAC rule (a data: URL) was tried first and does not
//     hold: Chromium 14x still sends loopback direct under it;
//   - every host name but 127.0.0.1 also resolves to nothing;
//   - WebRTC sends no UDP but through a proxy (and there is none), QUIC is
//     off, and so is every request the browser makes on its own
//     (background networking, component updates, sync, the crash reporter,
//     DNS prefetch);
//   - no sandbox of Chromium's own: the session is the sandbox, with every
//     capability dropped and no new privileges, where Chromium's needs
//     namespaces it is not given.
//
// No cookie, header or credential is passed: the page is loaded directly,
// never through the exposure route, so nothing of the user's reaches it.
var browserFlags = []string{
	"--headless=new",
	"--no-sandbox",
	"--disable-gpu",
	"--disable-dev-shm-usage",
	"--no-first-run",
	"--no-default-browser-check",
	"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
	"--disable-quic",
	"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
	"--webrtc-ip-handling-policy=disable_non_proxied_udp",
	"--disable-background-networking",
	"--disable-component-update",
	"--disable-sync",
	"--disable-extensions",
	"--disable-default-apps",
	"--disable-domain-reliability",
	"--disable-breakpad",
	"--dns-prefetch-disable",
	"--deny-permission-prompts",
	"--mute-audio",
	"--hide-scrollbars",
}

// browserScript runs one shot in the session: $1 is the address, $2 the
// viewport, the rest the browser's switches. Its profile, and every folder
// it might write to, is a fresh one in the session's /tmp (a tmpfs),
// removed when the script ends, so no cookie or storage outlives the shot
// and nothing lands in the workspace. The picture is printed on stdout.
const browserScript = `set -eu
url=$1; size=$2; shift 2
d=$(mktemp -d /tmp/loomcycle-shot.XXXXXX)
trap 'rm -rf "$d"' EXIT
HOME="$d" XDG_CONFIG_HOME="$d" XDG_CACHE_HOME="$d" chromium "$@" --user-data-dir="$d/profile" --window-size="$size" --screenshot="$d/shot.png" -- "$url" </dev/null >/dev/null 2>"$d/err" || { tail -c 512 "$d/err" >&2; exit 1; }
cat "$d/shot.png"`

// browserProxy is the proxy switches of a shot of port: every request goes
// to 0.0.0.0:9, where nothing answers (Chromium does not fall back to a
// direct connection when a fixed proxy fails), but the one bypass entry,
// 127.0.0.1 at port. A process of the session could bind that port and act
// as the proxy, but it would reach only what the session reaches, which a
// screenshot's grant (exec) already allows. The port is an int checked by
// ScreenshotURL, so no text of the call is written in.
func browserProxy(port int) []string {
	return []string{"--proxy-server=0.0.0.0:9", "--proxy-bypass-list=<-loopback>;127.0.0.1:" + strconv.Itoa(port)}
}

// screenshotArgv is the program a shot of url, on port, at width x height
// runs.
func screenshotArgv(url string, port, width, height int) []string {
	argv := []string{"/bin/sh", "-c", browserScript, "sh", url, strconv.Itoa(width) + "," + strconv.Itoa(height)}
	argv = append(argv, browserFlags...)
	return append(argv, browserProxy(port)...)
}

// pngSignature starts every PNG.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// screenshotOp takes a picture of a page a running server of workspace id
// serves (RFC GW §5.8, P10 plan §10). Only a container-engine worker with
// the browser label takes one; the host engine has no isolation to put a
// model-written page in. The address is checked again here, whatever the
// core sent, and the port must be one a running server of this workspace
// declared, as a request's must.
func (w *worker) screenshotOp(ctx context.Context, id, real string, raw []byte) answer {
	if w.opt.Engine != workspace.EngineContainer {
		return refuse(workspace.ClassBrowserUnavailable, "this worker runs the host engine, which offers no screenshot; it needs a container-engine worker with the browser image")
	}
	if !slices.Contains(w.opt.Labels, workspace.LabelBrowser) {
		return refuse(workspace.ClassBrowserUnavailable, "this worker's session image carries no browser")
	}
	var in workspace.ScreenshotArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	url, err := workspace.ScreenshotURL(in)
	if err != nil {
		return refuse(workspace.ClassInvalid, "%v", err)
	}
	// The core sends the viewport defaulted; zero here is refused, not
	// defaulted again, so the two ends cannot disagree on it.
	if in.Width == 0 || in.Height == 0 {
		return refuse(workspace.ClassInvalid, "the viewport's width and height are required")
	}
	width, height, err := workspace.ScreenshotViewport(in.Width, in.Height)
	if err != nil {
		return refuse(workspace.ClassInvalid, "%v", err)
	}
	if len(w.servers.serving(id, in.Port, false)) == 0 {
		return refuse(workspace.ClassPortNotServed, "no running server of this workspace serves port %d; start one with that port (logs lists them)", in.Port)
	}
	if _, busy := w.shots.LoadOrStore(id, struct{}{}); busy {
		return refuse(workspace.ClassBrowserBusy, "a screenshot of this workspace is already being taken; try again when it ends")
	}
	defer w.shots.Delete(id)
	p, err := w.engine.Start(ctx, engine.StartRequest{Workspace: id, Root: real, Dir: real, Argv: screenshotArgv(url, in.Port, width, height)})
	if err != nil {
		return refuse(workspace.ClassFailed, "starting the browser: %v", err)
	}
	defer func() { _ = p.Kill() }()
	type read struct {
		out []byte
		err error
	}
	got := make(chan read, 1)
	safego.Go(context.WithoutCancel(ctx), "workspace.worker.screenshot-read", func() {
		// One byte past the bound, so the core sees a picture too large
		// and refuses it whole, and the worker never holds more.
		var b bytes.Buffer
		_, err := io.Copy(&b, io.LimitReader(p.Stdout(), workspace.MaxScreenshotBytes+1))
		got <- read{out: b.Bytes(), err: err}
	})
	timer := time.NewTimer(w.screenshotTimeout())
	defer timer.Stop()
	var r read
	select {
	case r = <-got:
	case <-timer.C:
		_ = p.Kill()
		return refuse(workspace.ClassBrowserTimedOut, "the page did not load and draw within %v; the browser was killed", w.screenshotTimeout())
	case <-ctx.Done():
		_ = p.Kill()
		return refuse(workspace.ClassFailed, "%v", ctx.Err())
	}
	if r.err != nil && !errors.Is(r.err, io.EOF) {
		return refuse(workspace.ClassFailed, "reading the picture: %v", r.err)
	}
	if !bytes.HasPrefix(r.out, pngSignature) {
		// Its stderr is the browser's own log of a failed load, cut short.
		return refuse(workspace.ClassFailed, "the browser made no picture of %s: %s", url, tailString(p.Stderr(), 512))
	}
	return answer{output: r.out}
}

// screenshotTimeout is how long one shot may take.
func (w *worker) screenshotTimeout() time.Duration {
	if w.shotTimeout > 0 {
		return w.shotTimeout
	}
	return workspace.ScreenshotTimeout
}

// tailString is the last n bytes of s.
func tailString(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
