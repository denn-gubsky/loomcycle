package workspace

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// OpScreenshot is a picture of a page a server of the workspace serves,
// taken by a headless browser inside the workspace's own container session
// (RFC GW §5.8; P10 plan §10, owner ruling of 2026-11-26: option A, a
// second session image). The browser reaches the session's own loopback
// and nothing else.
const OpScreenshot = "screenshot"

// LabelBrowser is the label a container-engine worker reports when its
// session image carries the browser (Dockerfile.workspace, target browser),
// found by starting a session of it at the worker's start, as its
// toolchains are: no setting can claim a browser the image lacks. A worker
// without it is never asked for a screenshot.
const LabelBrowser = "browser"

// The screenshot's bounds (P10 plan §10).
const (
	ScreenshotDefaultWidth  = 1280
	ScreenshotDefaultHeight = 800
	ScreenshotMaxWidth      = 1920
	ScreenshotMaxHeight     = 1080
	// ScreenshotTimeout is how long one shot may take, the browser's start
	// included; past it the browser is killed.
	ScreenshotTimeout = 30 * time.Second
	// MaxScreenshotBytes bounds the picture the worker sends back. The core
	// then scales it to the model's image bounds (tools.MaxImageBytes,
	// tools.MaxImageEdge), as it does view's.
	MaxScreenshotBytes = 20 << 20
	// MaxScreenshotPath bounds the path, in bytes.
	MaxScreenshotPath = 2048
)

// The screenshot's refusals.
const (
	// ClassBrowserUnavailable: the worker offers no browser (no browser
	// label, or the host engine, which does not offer screenshot).
	ClassBrowserUnavailable = "browser_unavailable"
	// ClassBrowserBusy: a shot of this workspace is already being taken.
	ClassBrowserBusy = "browser_busy"
	// ClassBrowserTimedOut: the shot took longer than ScreenshotTimeout and
	// the browser was killed.
	ClassBrowserTimedOut = "browser_timed_out"
)

// ScreenshotArgs are a screenshot op's arguments. Width and Height are the
// viewport; the core sends them already defaulted (ScreenshotViewport).
type ScreenshotArgs struct {
	Port   int    `json:"port"`
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// ScreenshotViewport is the viewport for width and height as asked: zero is
// the default, and neither may pass the bound.
func ScreenshotViewport(width, height int) (int, int, error) {
	if width == 0 {
		width = ScreenshotDefaultWidth
	}
	if height == 0 {
		height = ScreenshotDefaultHeight
	}
	if width < 1 || width > ScreenshotMaxWidth || height < 1 || height > ScreenshotMaxHeight {
		return 0, 0, fmt.Errorf("the viewport is at most %dx%d", ScreenshotMaxWidth, ScreenshotMaxHeight)
	}
	return width, height, nil
}

// ScreenshotURL is the one address the browser loads for a: the session's
// own loopback, http://127.0.0.1:<port><path>. Only a path is taken, never a
// URL: it must start with one "/" and hold only printable ASCII other than
// "\" (a browser reads "\" as "/", and "//" as the start of a host). The
// result is parsed back, and refused unless its host is exactly the
// loopback and port, so no path can name another host.
func ScreenshotURL(a ScreenshotArgs) (string, error) {
	if a.Port < 1 || a.Port > 65535 {
		return "", fmt.Errorf("port must be from 1 to 65535")
	}
	p := a.Path
	switch {
	case p == "" || p[0] != '/':
		return "", fmt.Errorf("path must start with /")
	case len(p) > MaxScreenshotPath:
		return "", fmt.Errorf("path is over %d bytes", MaxScreenshotPath)
	case len(p) > 1 && p[1] == '/':
		return "", fmt.Errorf("path may not start with //")
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= ' ' || c >= 0x7f || c == '\\' {
			return "", fmt.Errorf("path may hold only printable ASCII without spaces or \\; percent-encode the rest")
		}
	}
	host := "127.0.0.1:" + strconv.Itoa(a.Port)
	raw := "http://" + host + p
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host != host || u.User != nil {
		return "", fmt.Errorf("path is not a path of the workspace's server")
	}
	return raw, nil
}
