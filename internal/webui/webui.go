// Package webui owns the embedded React SPA shipped in v0.7.3+.
//
// The package serves two HTTP handlers — the index (which optionally
// converts a `?token=` query into the loomcycle_session cookie) and
// a static asset handler with SPA fallback to index.html so React
// Router handles deep links like /ui/agents/{id}.
//
// Build output lives at `internal/webui/dist/` because `go:embed`
// can't reach `..` — Vite's `web/vite.config.ts` writes here. CI
// runs `make build-ui` before `go build`. When the dist directory
// is empty (operator skipped the npm build), every handler returns
// 503 with a `ui_not_built` code so the diagnostic is unambiguous.
package webui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SessionCookie is the cookie name the SPA uses to authenticate
// against /v1/* after the operator hits /ui?token=...
//
// Standard HttpOnly cookie — JavaScript in the SPA can't read it
// (no XSS exposure of the bearer token); fetch() includes it on
// same-origin requests automatically.
const SessionCookie = "loomcycle_session"

// uiAssets embeds the production build of the web/ React app.
//
//go:embed all:dist
var uiAssets embed.FS

// Handler returns the http.Handler that serves the embedded SPA at
// the given prefix (typically "/ui"). The handler:
//
//   - GET <prefix>?token=...   sets SessionCookie + 302s back to
//     <prefix>; subsequent /v1/* calls authenticate via the cookie.
//   - GET <prefix>             serves index.html.
//   - GET <prefix>/<file>      serves embedded asset OR falls back
//     to index.html (SPA-router pattern).
//
// The `secureCookie` flag forces the Secure attribute on the
// session cookie. Operators behind TLS terminators that don't set
// r.TLS should pass true.
func Handler(prefix string, secureCookie bool) http.Handler {
	prefix = strings.TrimRight(prefix, "/")
	mux := http.NewServeMux()

	mux.HandleFunc(fmt.Sprintf("GET %s", prefix), func(w http.ResponseWriter, r *http.Request) {
		// Token-set redirect — operator's first load.
		if token := r.URL.Query().Get("token"); token != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     SessionCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				Secure:   secureCookie || r.TLS != nil,
			})
			http.Redirect(w, r, prefix, http.StatusFound)
			return
		}
		serveFile(w, "index.html")
	})

	// Logout — clears the session cookie and bounces to the login page. The
	// cookie is HttpOnly, so JS can't clear it; this server route is the only
	// way to end a Web UI session. A full navigation to <prefix>/logout (not a
	// router push) runs this handler before React Router sees the path. More
	// specific than the catch-all below, so Go 1.22 ServeMux routes here first.
	mux.HandleFunc(fmt.Sprintf("GET %s/logout", prefix), func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookie,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   secureCookie || r.TLS != nil,
			MaxAge:   -1, // delete now
		})
		http.Redirect(w, r, prefix+"/login", http.StatusFound)
	})

	mux.HandleFunc(fmt.Sprintf("GET %s/", prefix), func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, prefix+"/")
		if rel == "" {
			serveFile(w, "index.html")
			return
		}
		// Path traversal guard. embed.FS would resolve safely, but
		// rejecting at the boundary keeps the audit story clean.
		if strings.Contains(rel, "..") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		// Try the embedded file; on miss, fall back to index.html so
		// React Router takes over (deep links to /ui/agents/{id}).
		if info, err := fs.Stat(uiAssets, "dist/"+rel); err == nil && !info.IsDir() {
			serveFile(w, rel)
			return
		}
		serveFile(w, "index.html")
	})

	return mux
}

// serveFile reads a file from the embedded fs and writes it with a
// sensible content-type. Returns 503 with a diagnostic code when
// dist/ is empty (UI not built).
func serveFile(w http.ResponseWriter, name string) {
	data, err := fs.ReadFile(uiAssets, "dist/"+name)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"code":"ui_not_built","error":"web UI not built; run \"make build-ui\" (or \"cd web && npm install && npm run build\") before starting loomcycle"}`)
		return
	}
	if ct := contentType(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if strings.HasPrefix(name, "assets/") {
		// Vite emits content-hashed filenames under assets/, safe to
		// cache forever.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// index.html and the like — never cache so the operator's
		// reload picks up a fresh shell after a UI redeploy.
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(data)
}

// contentType returns the right Content-Type for an embedded file.
// Hand-rolled because the embedded fs doesn't go through OS mime
// tables and the set we ship is small enough to enumerate.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".woff", ".woff2":
		return "font/woff2"
	case ".map":
		return "application/json"
	}
	return ""
}
