package http

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// RFC HI P3 acceptance (G35), in a real browser: an
// <iframe sandbox="allow-scripts allow-forms"> pointed at a frame path loads
// a multi-file application (HTML, a script, a stylesheet, an image) whose
// fetch to a relative API path and WebSocket to a relative path both work,
// with no Authorization header anywhere.
//
// The route's sandbox gives the page an opaque origin, so its fetch to
// loomcycle's origin is cross-origin: the frame path answers it with
// Access-Control-Allow-Origin: * (the owner's ruling on RFC HI P3, option a),
// and a fetch with a preflight is answered by the route itself.
//
// It needs a Chrome or Chromium: LOOMCYCLE_TEST_CHROME names one, else the
// usual places are tried. Without one it is skipped (CI has none); the
// header-free HTTP and socket path it drives is covered without a browser by
// TestExposureFrame_ServesThePageWithoutAHeader and
// TestExposureFrame_OpensTheSocketWithoutACredential.
func TestExposureFrame_ASandboxedIframeRunsAMultiFileApp(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a browser")
	}
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no Chrome or Chromium (set LOOMCYCLE_TEST_CHROME)")
	}
	tf, ts := frameServer(t)

	type seen struct{ method, path, auth, referer string }
	var (
		mu       sync.Mutex
		requests []seen
	)
	report := make(chan url.Values, 1)
	png, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	socket := echoSocket(&wsSeen{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, seen{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Referer")})
		mu.Unlock()
		switch r.URL.Path {
		case "/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!doctype html><html><head><link rel="stylesheet" href="style.css"></head>`+
				`<body><img id="i" src="img.png"><script src="app.js"></script></body></html>`)
		case "/style.css":
			w.Header().Set("Content-Type", "text/css")
			_, _ = io.WriteString(w, `body { color: rgb(1, 2, 3); }`)
		case "/img.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		case "/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(w, frameAppScript)
		case "/api/data":
			if r.Method == http.MethodPost {
				b, _ := io.ReadAll(r.Body)
				_, _ = io.WriteString(w, "posted:"+string(b))
				return
			}
			_, _ = io.WriteString(w, "data-ok")
		case "/ws":
			socket(w, r)
		case "/report":
			select {
			case report <- r.URL.Query():
			default:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(app.Close)
	tf.f.app = app

	m := mustFrame(t, ts, "alice-token", testExposureID)
	frameCap := capOf(t, m.FramePath)
	// The embedding site, on another origin than loomcycle's.
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><iframe sandbox="allow-scripts allow-forms" src="%s%sindex.html"></iframe>`, ts.URL, m.FramePath)
	}))
	t.Cleanup(board.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--user-data-dir="+t.TempDir(), board.URL)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the browser: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	var got url.Values
	select {
	case got = <-report:
	case <-ctx.Done():
		mu.Lock()
		asked := append([]seen(nil), requests...)
		mu.Unlock()
		t.Fatalf("the frame's application never reported; it was asked %+v", asked)
	}
	for _, k := range []string{"css", "img", "fetch", "post", "ws"} {
		if got.Get(k) != "ok" {
			t.Errorf("in the frame %s = %q, want ok (report %v)", k, got.Get(k), got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"/index.html", "/style.css", "/img.png", "/app.js", "/api/data", "/ws"} {
		found := false
		for _, r := range requests {
			found = found || strings.HasPrefix(r.path, want)
		}
		if !found {
			t.Errorf("the application was never asked %s: %+v", want, requests)
		}
	}
	for _, r := range requests {
		if r.method == http.MethodOptions {
			t.Errorf("the preflight reached the application: %+v", r)
		}
		if r.auth != "" || strings.Contains(r.path, frameCap) || strings.Contains(r.referer, frameCap) || strings.Contains(r.path, "/~f/") {
			t.Errorf("the application received a credential or the capability: %+v", r)
		}
	}
}

// frameAppScript checks the frame's own assets loaded, then fetches a
// relative API path and opens a relative socket, and reports all four by a
// relative request.
const frameAppScript = `
(async () => {
  const out = {};
  await new Promise(r => document.readyState === "complete" ? r() : addEventListener("load", r));
  out.css = getComputedStyle(document.body).color === "rgb(1, 2, 3)" ? "ok" : "no";
  out.img = document.getElementById("i").naturalWidth === 1 ? "ok" : "no";
  try { out.fetch = (await (await fetch("api/data")).text()) === "data-ok" ? "ok" : "no"; } catch (e) { out.fetch = String(e); }
  // A JSON POST is not a simple request: the browser sends a preflight first.
  try {
    const r = await fetch("api/data", {method: "POST", headers: {"Content-Type": "application/json", "X-App": "1"}, body: "{\"a\":1}"});
    out.post = (await r.text()) === "posted:{\"a\":1}" ? "ok" : "no";
  } catch (e) { out.post = String(e); }
  try {
    out.ws = await new Promise((resolve) => {
      const ws = new WebSocket(new URL("ws", location.href).href.replace(/^http/, "ws"), ["loomcycle.surface.v1"]);
      const t = setTimeout(() => resolve("timeout"), 10000);
      ws.onopen = () => ws.send("ping");
      ws.onmessage = (ev) => { clearTimeout(t); resolve(ev.data === "ping" ? "ok" : "no"); ws.close(); };
      ws.onerror = () => { clearTimeout(t); resolve("error"); };
    });
  } catch (e) { out.ws = String(e); }
  await fetch("report?" + new URLSearchParams(out));
})();
`

// findChrome is a Chrome or Chromium to drive, or "".
func findChrome() string {
	if p := os.Getenv("LOOMCYCLE_TEST_CHROME"); p != "" {
		return p
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}
