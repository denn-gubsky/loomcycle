package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testFiles is a minimal built SPA: a shell, a content-hashed asset and a
// non-asset file, which together exercise both caching rules.
func testFiles() map[string]string {
	return map[string]string{
		"index.html":           "<!doctype html><title>bundle</title>",
		"assets/app-abc123.js": "console.log('bundle')",
		"favicon.svg":          "<svg/>",
	}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// manifestFor lists every file of files with its true digest, for the given
// API level range.
func manifestFor(files map[string]string, minLevel, maxLevel int) map[string]any {
	digests := map[string]string{}
	for name, body := range files {
		digests[name] = digest(body)
	}
	return map[string]any{
		"ui_version":    "1.2.3",
		"api_level_min": minLevel,
		"api_level_max": maxLevel,
		"files":         digests,
	}
}

// writeBundle writes files and a manifest into a fresh directory. The manifest
// is passed in already built so a test can corrupt it; nil means the correct
// manifest for files at the binary's own level.
func writeBundle(t *testing.T, files map[string]string, manifest any) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if manifest == nil {
		manifest = manifestFor(files, APILevel, APILevel)
	}
	var raw []byte
	if s, ok := manifest.(string); ok {
		raw = []byte(s)
	} else {
		var err error
		if raw, err = json.Marshal(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
	return rec
}

func mustLoad(t *testing.T, dir string) *Bundle {
	t.Helper()
	b, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return b
}

func TestBundle_ValidBundleIsServedInsteadOfEmbedded(t *testing.T) {
	files := testFiles()
	b := mustLoad(t, writeBundle(t, files, nil))
	if b.UIVersion() != "1.2.3" {
		t.Errorf("UIVersion = %q, want 1.2.3", b.UIVersion())
	}
	h := Handler("/ui", false, nil, WithBundle(b))

	cases := []struct {
		target, body, contentType, cacheControl string
	}{
		{"/ui", files["index.html"], "text/html; charset=utf-8", "no-cache"},
		{"/ui/", files["index.html"], "text/html; charset=utf-8", "no-cache"},
		{"/ui/assets/app-abc123.js", files["assets/app-abc123.js"], "application/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/ui/favicon.svg", files["favicon.svg"], "image/svg+xml", "no-cache"},
		// A deep link React Router owns falls back to the bundle's shell.
		{"/ui/agents/abc", files["index.html"], "text/html; charset=utf-8", "no-cache"},
		// The manifest is not part of the SPA and is never served.
		{"/ui/manifest.json", files["index.html"], "text/html; charset=utf-8", "no-cache"},
	}
	for _, c := range cases {
		rec := get(t, h, c.target)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", c.target, rec.Code)
			continue
		}
		if rec.Body.String() != c.body {
			t.Errorf("%s: body = %q, want %q", c.target, rec.Body.String(), c.body)
		}
		if got := rec.Header().Get("Content-Type"); got != c.contentType {
			t.Errorf("%s: Content-Type = %q, want %q", c.target, got, c.contentType)
		}
		if got := rec.Header().Get("Cache-Control"); got != c.cacheControl {
			t.Errorf("%s: Cache-Control = %q, want %q", c.target, got, c.cacheControl)
		}
	}
}

// TestBundle_HeadersMatchEmbeddedRules pins that a bundle file and the same
// file from the embedded source get identical headers: both go through
// writeFile, never a bundle-specific path.
func TestBundle_HeadersMatchEmbeddedRules(t *testing.T) {
	for _, name := range []string{"index.html", "assets/app-abc123.js", "favicon.svg", "assets/x.woff2", "robots.txt"} {
		a, bb := httptest.NewRecorder(), httptest.NewRecorder()
		writeFile(a, name, []byte("x"))
		b := &Bundle{files: map[string][]byte{name: []byte("x")}}
		serveFile(bb, b, name)
		for _, h := range []string{"Content-Type", "Cache-Control"} {
			if a.Header().Get(h) != bb.Header().Get(h) {
				t.Errorf("%s %s: shared rule %q, bundle %q", name, h, a.Header().Get(h), bb.Header().Get(h))
			}
		}
	}
}

func TestBundle_RefusalsFallBackWithReason(t *testing.T) {
	outside := t.TempDir()
	secret := "outside the bundle"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}

	type tc struct {
		name   string
		dir    func(t *testing.T) string
		reason string
	}
	withManifest := func(edit func(m map[string]any)) func(t *testing.T) string {
		return func(t *testing.T) string {
			files := testFiles()
			m := manifestFor(files, APILevel, APILevel)
			edit(m)
			return writeBundle(t, files, m)
		}
	}
	withPath := func(p string) func(t *testing.T) string {
		return withManifest(func(m map[string]any) {
			m["files"].(map[string]string)[p] = digest("x")
		})
	}
	cases := []tc{
		{"api level below the bundle's minimum", withManifest(func(m map[string]any) {
			m["api_level_min"], m["api_level_max"] = APILevel+1, APILevel+3
		}), ReasonAPILevel},
		// Above the maximum: TestBundle_LevelAboveMaxIsRefused (needs a
		// level above 1, so it calls loadDir with one).
		{"digest mismatch", func(t *testing.T) string {
			files := testFiles()
			m := manifestFor(files, APILevel, APILevel)
			files["assets/app-abc123.js"] = "console.log('tampered')"
			return writeBundle(t, files, m)
		}, ReasonDigestMismatch},
		{"listed file missing", withManifest(func(m map[string]any) {
			m["files"].(map[string]string)["assets/gone.js"] = digest("gone")
		}), ReasonFileMissing},
		{"unlisted extra file", func(t *testing.T) string {
			dir := writeBundle(t, testFiles(), nil)
			if err := os.WriteFile(filepath.Join(dir, "assets", "extra.js"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return dir
		}, ReasonFileUnlisted},
		{"unlisted hidden file", func(t *testing.T) string {
			dir := writeBundle(t, testFiles(), nil)
			if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return dir
		}, ReasonFileUnlisted},
		{"listed symlink pointing inside", func(t *testing.T) string {
			files := testFiles()
			m := manifestFor(files, APILevel, APILevel)
			// The link's target has exactly the listed digest, so only the
			// link check can refuse it.
			m["files"].(map[string]string)["alias.js"] = digest(files["assets/app-abc123.js"])
			dir := writeBundle(t, files, m)
			if err := os.Symlink("assets/app-abc123.js", filepath.Join(dir, "alias.js")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, ReasonFileNotRegular},
		{"listed symlink pointing outside", func(t *testing.T) string {
			files := testFiles()
			m := manifestFor(files, APILevel, APILevel)
			m["files"].(map[string]string)["leak.txt"] = digest(secret)
			dir := writeBundle(t, files, m)
			if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "leak.txt")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, ReasonFileNotRegular},
		{"symlinked directory pointing outside", func(t *testing.T) string {
			files := testFiles()
			m := manifestFor(files, APILevel, APILevel)
			m["files"].(map[string]string)["ext/secret.txt"] = digest(secret)
			dir := writeBundle(t, files, m)
			if err := os.Symlink(outside, filepath.Join(dir, "ext")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, ReasonFileNotRegular},
		{"manifest is a symlink", func(t *testing.T) string {
			files := testFiles()
			dir := writeBundle(t, files, nil)
			real := filepath.Join(outside, "manifest.json")
			raw, _ := json.Marshal(manifestFor(files, APILevel, APILevel))
			if err := os.WriteFile(real, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, ReasonFileNotRegular},
		{"path with ..", withPath("../secret.txt"), ReasonPathInvalid},
		{"path with inner ..", withPath("assets/../../secret.txt"), ReasonPathInvalid},
		{"absolute path", withPath("/etc/passwd"), ReasonPathInvalid},
		{"backslash path", withPath(`assets\app.js`), ReasonPathInvalid},
		{"dot-slash path", withPath("./index.html"), ReasonPathInvalid},
		{"empty element", withPath("assets//app.js"), ReasonPathInvalid},
		{"empty path", withPath(""), ReasonPathInvalid},
		{"control character", withPath("assets/a\nb.js"), ReasonPathInvalid},
		{"manifest lists itself", withPath("manifest.json"), ReasonPathInvalid},
		{"malformed manifest", func(t *testing.T) string {
			return writeBundle(t, testFiles(), `{"ui_version": "1.2.3",`)
		}, ReasonManifestInvalid},
		{"unknown manifest field", withManifest(func(m map[string]any) {
			m["entrypoint"] = "index.html"
		}), ReasonManifestInvalid},
		{"trailing data after the manifest", func(t *testing.T) string {
			files := testFiles()
			raw, _ := json.Marshal(manifestFor(files, APILevel, APILevel))
			return writeBundle(t, files, string(raw)+`{}`)
		}, ReasonManifestInvalid},
		{"uppercase digest", withManifest(func(m map[string]any) {
			f := m["files"].(map[string]string)
			f["index.html"] = strings.ToUpper(f["index.html"])
		}), ReasonManifestInvalid},
		{"short digest", withManifest(func(m map[string]any) {
			m["files"].(map[string]string)["index.html"] = "abcd"
		}), ReasonManifestInvalid},
		{"ui_version with markup", withManifest(func(m map[string]any) {
			m["ui_version"] = "<script>alert(1)</script>"
		}), ReasonManifestInvalid},
		{"missing ui_version", withManifest(func(m map[string]any) {
			delete(m, "ui_version")
		}), ReasonManifestInvalid},
		{"min above max", withManifest(func(m map[string]any) {
			m["api_level_min"], m["api_level_max"] = 3, 2
		}), ReasonManifestInvalid},
		{"level zero", withManifest(func(m map[string]any) {
			m["api_level_min"] = 0
		}), ReasonManifestInvalid},
		{"no index.html", func(t *testing.T) string {
			files := testFiles()
			delete(files, "index.html")
			return writeBundle(t, files, manifestFor(files, APILevel, APILevel))
		}, ReasonIndexMissing},
		{"no manifest", func(t *testing.T) string {
			dir := writeBundle(t, testFiles(), nil)
			if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, ReasonManifestInvalid},
		{"directory does not exist", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "nope")
		}, ReasonDirUnreadable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.dir(t)
			b, st, err := LoadConfigured(dir)
			if err == nil {
				t.Fatalf("loaded, want refusal %q", c.reason)
			}
			if b != nil {
				t.Errorf("bundle returned alongside an error")
			}
			var le *LoadError
			if !errors.As(err, &le) || le.Reason != c.reason {
				t.Fatalf("err = %v, want reason %q", err, c.reason)
			}
			want := Status{Source: SourceEmbedded, FallbackReason: c.reason}
			if st != want {
				t.Errorf("status = %+v, want %+v", st, want)
			}
		})
	}
}

// TestBundle_LevelAboveMaxIsRefused: a binary whose API moved past the newest
// level a bundle supports must not serve it.
func TestBundle_LevelAboveMaxIsRefused(t *testing.T) {
	files := testFiles()
	dir := writeBundle(t, files, manifestFor(files, 1, 3))
	if _, err := loadDir(dir, 3, defaultLimits); err != nil {
		t.Fatalf("level at the maximum refused: %v", err)
	}
	if _, err := loadDir(dir, 1, defaultLimits); err != nil {
		t.Fatalf("level at the minimum refused: %v", err)
	}
	_, err := loadDir(dir, 4, defaultLimits)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != ReasonAPILevel {
		t.Fatalf("level above max: err = %v, want %q", err, ReasonAPILevel)
	}
}

func TestBundle_OversizeIsRefused(t *testing.T) {
	files := testFiles()
	dir := writeBundle(t, files, nil)
	total := 0
	for _, body := range files {
		total += len(body)
	}
	cases := []struct {
		name   string
		lim    limits
		reason string
	}{
		{"total bytes", limits{manifestBytes: 1 << 20, bundleBytes: int64(total) - 1, files: 100}, ReasonBundleTooLarge},
		{"file count", limits{manifestBytes: 1 << 20, bundleBytes: 1 << 20, files: len(files) - 1}, ReasonBundleTooLarge},
		{"manifest bytes", limits{manifestBytes: 16, bundleBytes: 1 << 20, files: 100}, ReasonManifestTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadDir(dir, APILevel, c.lim)
			var le *LoadError
			if !errors.As(err, &le) || le.Reason != c.reason {
				t.Fatalf("err = %v, want %q", err, c.reason)
			}
		})
	}
	// Exactly at every limit loads.
	at := limits{manifestBytes: 1 << 20, bundleBytes: int64(total), files: len(files)}
	if _, err := loadDir(dir, APILevel, at); err != nil {
		t.Fatalf("bundle exactly at the limits refused: %v", err)
	}
}

// TestBundle_FileChangedOnDiskAfterLoadIsNotServed proves the bundle is held
// in memory: rewriting (or deleting) a file after the check cannot change
// what the browser gets, so there is no gap between verifying and serving.
func TestBundle_FileChangedOnDiskAfterLoadIsNotServed(t *testing.T) {
	files := testFiles()
	dir := writeBundle(t, files, nil)
	h := Handler("/ui", false, nil, WithBundle(mustLoad(t, dir)))

	if err := os.WriteFile(filepath.Join(dir, "assets", "app-abc123.js"), []byte("evil()"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if got := get(t, h, "/ui/assets/app-abc123.js").Body.String(); got != files["assets/app-abc123.js"] {
		t.Errorf("asset after rewrite = %q, want the verified bytes", got)
	}
	if got := get(t, h, "/ui").Body.String(); got != files["index.html"] {
		t.Errorf("index after delete = %q, want the verified bytes", got)
	}
}

// TestBundle_GoRoutesAreNotShadowedByBundleFiles: files named like the Go
// routes do not replace them — the session, login-config and logout routes
// keep their behaviour with a bundle present.
func TestBundle_GoRoutesAreNotShadowedByBundleFiles(t *testing.T) {
	files := testFiles()
	files["session"] = "bundle session"
	files["login-config.json"] = `{"login_origins":["https://evil.example"]}`
	files["logout"] = "bundle logout"
	h := Handler("/ui", false, []string{"https://loomcycle.cloud"}, WithBundle(mustLoad(t, writeBundle(t, files, nil))))

	rec := get(t, h, "/ui/login-config.json")
	if strings.Contains(rec.Body.String(), "evil.example") || !strings.Contains(rec.Body.String(), "https://loomcycle.cloud") {
		t.Errorf("login-config served from the bundle: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("login-config Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}

	rec = get(t, h, "/ui/logout")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Errorf("logout: status %d Location %q, want 302 /ui/login", rec.Code, rec.Header().Get("Location"))
	}

	req := httptest.NewRequest("POST", "/ui/session", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) != 1 || rec.Result().Cookies()[0].Value != "tok" {
		t.Errorf("POST session: status %d cookies %v, want 204 + session cookie", rec.Code, rec.Result().Cookies())
	}

	req = httptest.NewRequest("POST", "/ui/session", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST session: status %d, want 403", rec.Code)
	}

	rec = get(t, h, "/ui?token=abc")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui" {
		t.Errorf("?token= redirect: status %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestLoadConfigured_UnsetServesEmbedded(t *testing.T) {
	b, st, err := LoadConfigured("")
	if b != nil || err != nil {
		t.Fatalf("unset: bundle %v err %v, want nil, nil", b, err)
	}
	if st != (Status{Source: SourceEmbedded}) {
		t.Errorf("status = %+v, want embedded with no reason", st)
	}
}

func TestLoadConfigured_ValidBundleReportsDirSource(t *testing.T) {
	b, st, err := LoadConfigured(writeBundle(t, testFiles(), nil))
	if err != nil || b == nil {
		t.Fatalf("LoadConfigured: %v", err)
	}
	if st != (Status{Source: SourceDir, UIVersion: "1.2.3"}) {
		t.Errorf("status = %+v", st)
	}
}

// TestLoadConfigured_ReasonCarriesNoPathOrContent: the status is served by the
// unauthenticated /healthz, so it carries a fixed reason code — never the
// configured directory, a file name from the manifest, or file bytes. Those
// belong in the error, which only the startup log prints.
func TestLoadConfigured_ReasonCarriesNoPathOrContent(t *testing.T) {
	files := testFiles()
	m := manifestFor(files, APILevel, APILevel)
	files["assets/app-abc123.js"] = "TAMPERED-CONTENT"
	dir := writeBundle(t, files, m)
	_, st, err := LoadConfigured(dir)
	if err == nil {
		t.Fatal("tampered bundle loaded")
	}
	raw, _ := json.Marshal(st)
	for _, leak := range []string{dir, "app-abc123", "TAMPERED"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("status %s leaks %q", raw, leak)
		}
	}
	if !strings.Contains(err.Error(), "assets/app-abc123.js") {
		t.Errorf("log error %q should name the offending file", err)
	}
}
