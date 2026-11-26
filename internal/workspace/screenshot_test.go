package workspace

import (
	"strings"
	"testing"
)

// The browser loads one address: the session's own loopback at the port and
// path asked for. Whatever the path says, the host stays 127.0.0.1:<port>.
func TestScreenshotURL_IsAlwaysTheSessionsOwnLoopback(t *testing.T) {
	for path, want := range map[string]string{
		"/":                   "http://127.0.0.1:8000/",
		"/report?page=2#top":  "http://127.0.0.1:8000/report?page=2#top",
		"/a/b%20c/index.html": "http://127.0.0.1:8000/a/b%20c/index.html",
		"/@evil.example.com/": "http://127.0.0.1:8000/@evil.example.com/",
	} {
		got, err := ScreenshotURL(ScreenshotArgs{Port: 8000, Path: path})
		if err != nil || got != want {
			t.Errorf("ScreenshotURL(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
}

// Anything that is not a path of the workspace's own server is refused: an
// external URL, a scheme-relative or backslash form a browser could read as
// another host, a path that does not start with /, and bytes a URL parser
// reads specially.
func TestScreenshotURL_RefusesAnythingButALocalPath(t *testing.T) {
	for _, path := range []string{
		"",
		"http://evil.example.com/",
		"https://127.0.0.1:8000/",
		"evil.example.com/",
		"report",
		// These keep the host 127.0.0.1:<port> when glued on, so only the
		// leading-slash rule refuses them (the host re-check would not).
		"?x=1",
		"#top",
		"//evil.example.com/",
		"/\\evil.example.com",
		"\\\\evil.example.com",
		"/a b",
		"/a\tb",
		"/a\nb",
		"/café",
		"/" + strings.Repeat("a", MaxScreenshotPath),
	} {
		if got, err := ScreenshotURL(ScreenshotArgs{Port: 8000, Path: path}); err == nil {
			t.Errorf("ScreenshotURL(%q) = %q, want a refusal", path, got)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := ScreenshotURL(ScreenshotArgs{Port: port, Path: "/"}); err == nil {
			t.Errorf("port %d was accepted", port)
		}
	}
}

// The viewport defaults to 1280x800 and may be at most 1920x1080.
func TestScreenshotViewport_DefaultsAndBounds(t *testing.T) {
	w, h, err := ScreenshotViewport(0, 0)
	if err != nil || w != 1280 || h != 800 {
		t.Errorf("default = %dx%d, %v; want 1280x800", w, h, err)
	}
	if w, h, err := ScreenshotViewport(1920, 1080); err != nil || w != 1920 || h != 1080 {
		t.Errorf("the largest = %dx%d, %v", w, h, err)
	}
	for _, v := range [][2]int{{1921, 800}, {1280, 1081}, {-1, 800}, {1280, -5}} {
		if _, _, err := ScreenshotViewport(v[0], v[1]); err == nil {
			t.Errorf("viewport %dx%d was accepted", v[0], v[1])
		}
	}
}
