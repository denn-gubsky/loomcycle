package http

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RFC FA §8 "bearer in the browser": a census of the source, so a later change
// cannot quietly put a bearer back into a cookie.
//
// Go: the legacy bearer cookie may only be declared, read (token mode, one
// release) and deleted. Every non-comment mention of it outside those lines
// fails, and so does any literal "loomcycle_session" outside the declaration.
// Web UI: no source touches document.cookie, and nothing writes a token to web
// storage.

// repoRoot is the module root, three directories up from this package.
const repoRoot = "../../.."

// legacyCookieUses are the only lines that may mention the legacy cookie.
var legacyCookieUses = []string{
	`const LegacyBearerCookie = "loomcycle_session"`,
	`r.Cookie(webui.LegacyBearerCookie)`,
	`expiredCookie(webui.LegacyBearerCookie, secure)`,
	`[]string{sessionCookieSecure, sessionCookiePlain, webui.LegacyBearerCookie}`,
	`"auth: a browser signed in with the "+webui.LegacyBearerCookie+`,
}

func TestCensus_NoGoCodeSetsTheBearerCookie(t *testing.T) {
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for n := 1; sc.Scan(); n++ {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "//") {
					continue
				}
				if !strings.Contains(line, "LegacyBearerCookie") && !strings.Contains(line, "loomcycle_session") {
					continue
				}
				allowed := false
				for _, use := range legacyCookieUses {
					allowed = allowed || strings.Contains(line, use)
				}
				if !allowed {
					t.Errorf("%s:%d mentions the legacy bearer cookie outside its declaration, read and deletion: %s", path, n, line)
				}
			}
			return sc.Err()
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}

func TestCensus_WebUIKeepsNoCredentialInTheBrowser(t *testing.T) {
	root := filepath.Join(repoRoot, "web", "src")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("no Web UI source at %s", root)
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		ext := filepath.Ext(path)
		if d.IsDir() || (ext != ".ts" && ext != ".tsx") || strings.Contains(path, ".test.") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		if strings.Contains(src, "document.cookie") {
			t.Errorf("%s touches document.cookie; the session cookie is HttpOnly and the browser holds no bearer", path)
		}
		for _, line := range strings.Split(src, "\n") {
			l := strings.ToLower(line)
			if (strings.Contains(l, "localstorage.setitem") || strings.Contains(l, "sessionstorage.setitem")) &&
				(strings.Contains(l, "token") || strings.Contains(l, "bearer")) {
				t.Errorf("%s writes a token to web storage: %s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk web/src: %v", err)
	}
}
