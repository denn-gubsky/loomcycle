package webui

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const testInlineScript = "\n      document.documentElement.setAttribute('data-theme', 'dark');\n    "

// testBundle is a bundle whose index.html carries one inline script (as the
// built SPA's theme bootstrap does) and one external module.
func testBundle() *Bundle {
	return &Bundle{version: "test", files: map[string][]byte{
		"index.html": []byte(`<!doctype html><html><head><script>` + testInlineScript +
			`</script><script type="module" crossorigin src="/ui/assets/index-x.js"></script></head><body><div id="root"></div></body></html>`),
		"assets/index-x.js":  []byte("console.log(1)"),
		"assets/index-x.css": []byte("body{}"),
		"favicon.svg":        []byte("<svg/>"),
	}}
}

// spaPages are the client-side routes the SPA declares in web/src/main.tsx
// (each <Route path="…">), as /ui/… paths: every one of them is a page the
// handler answers with index.html, so the list is read, not copied.
func spaPages(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "main.tsx"))
	if err != nil {
		t.Fatalf("read the SPA router: %v", err)
	}
	var pages []string
	for _, m := range regexp.MustCompile(`<Route\b[^>]*?\bpath="([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		p := strings.TrimPrefix(m[1], "/")
		if p == "*" {
			continue
		}
		p = regexp.MustCompile(`:([A-Za-z]+)\??`).ReplaceAllString(p, "$1")
		pages = append(pages, "/ui/"+p)
	}
	for _, signIn := range []string{"/ui/login", "/ui/invite", "/ui/reset", "/ui/pending", "/ui/logout"} {
		found := false
		for _, p := range pages {
			found = found || p == signIn
		}
		if !found {
			t.Fatalf("the router no longer declares %s; the census below would not cover it", signIn)
		}
	}
	return pages
}

// TestWebUI_SecurityHeadersOnEveryPage: every response the Web UI handler
// writes — each page the SPA routes, the shell, its assets, the login config,
// the ?token= redirect, a 503 with no UI built — carries a CSP that forbids
// framing and allows no inline script but the shell's own (by hash), plus
// X-Frame-Options, nosniff and a same-origin Referrer-Policy.
func TestWebUI_SecurityHeadersOnEveryPage(t *testing.T) {
	sum := sha256.Sum256([]byte(testInlineScript))
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	targets := append(spaPages(t), "/ui", "/ui/", "/ui/login-config.json", "/ui/assets/index-x.js",
		"/ui/assets/index-x.css", "/ui/favicon.svg", "/ui?token=x")
	for _, h := range []struct {
		name string
		h    http.Handler
	}{{"bundle", Handler("/ui", nil, WithBundle(testBundle()))}, {"embedded", Handler("/ui", nil)}} {
		for _, target := range targets {
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
			hd := rec.Header()
			csp := hd.Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("%s GET %s: CSP %q lacks %q", h.name, target, csp, want)
				}
			}
			script := regexp.MustCompile(`script-src ([^;]*)`).FindStringSubmatch(csp)
			if script == nil || strings.Contains(script[1], "unsafe-inline") || strings.Contains(script[1], "unsafe-eval") {
				t.Errorf("%s GET %s: script-src %q must not allow inline or eval", h.name, target, script)
			}
			if h.name == "bundle" && (script == nil || !strings.Contains(script[1], wantHash)) {
				t.Errorf("%s GET %s: script-src %q lacks the shell's inline script hash %s", h.name, target, script, wantHash)
			}
			if hd.Get("X-Frame-Options") != "DENY" || hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "same-origin" {
				t.Errorf("%s GET %s: XFO %q nosniff %q referrer %q", h.name, target,
					hd.Get("X-Frame-Options"), hd.Get("X-Content-Type-Options"), hd.Get("Referrer-Policy"))
			}
		}
	}
}

// TestInlineScriptHashes_HashesOnlyInlineScripts: a script with src is not
// hashed, attributes and case do not hide an inline one, and the hash is over
// the exact text between the tags.
func TestInlineScriptHashes_HashesOnlyInlineScripts(t *testing.T) {
	html := `<script src="/a.js"></script><SCRIPT type="text/javascript">a()</SCRIPT><script>b()</script ><script
src="/b.js">ignored()</script>`
	got := InlineScriptHashes([]byte(html))
	var want []string
	for _, body := range []string{"a()", "b()"} {
		sum := sha256.Sum256([]byte(body))
		want = append(want, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("hashes = %v, want %v", got, want)
	}
}
