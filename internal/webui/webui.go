// Package webui owns the embedded React SPA shipped in v0.7.3+.
//
// The package serves the index, the unauthenticated login configuration and a
// static asset handler with SPA fallback to index.html so React Router handles
// deep links like /ui/agents/{id}. It sets no cookie: signing in and out are
// the API server's routes (RFC FA), so a UI bundle cannot change them.
//
// Build output lives at `internal/webui/dist/` because `go:embed`
// can't reach `..` — Vite's `web/vite.config.ts` writes here. CI
// runs `make build-ui` before `go build`. When the dist directory
// is empty (operator skipped the npm build), every handler returns
// 503 with a `ui_not_built` code so the diagnostic is unambiguous.
package webui

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// LegacyBearerCookie is the cookie earlier releases kept the raw API bearer
// in. Nothing sets it any more; token mode still reads it for one release
// (RFC FA decision 2) and logout clears it.
const LegacyBearerCookie = "loomcycle_session"

// LoginConfig is what the login page may know before anyone signs in: which
// gates to offer and nothing else about the deployment (RFC FA §8).
type LoginConfig struct {
	// Origins are the first-party origins the postMessage handoff may accept
	// a bearer from; empty disables the receiver.
	Origins []string
	// Mode is open, token or strict.
	Mode  string
	Gates []LoginGate
	// SessionRefused names why this request cannot start a session (e.g.
	// "insecure_transport"); empty when it can.
	SessionRefused string
	// App is the app an OAuth sign-in (?oauth=) is for, so the page can say
	// "Sign in to <app> at <destination>"; nil for a plain sign-in.
	App *LoginApp
}

// LoginApp names the app a sign-in will hand the person to.
type LoginApp struct {
	Label       string `json:"label"`
	Destination string `json:"destination"`
}

// LoginGate is one sign-in option on the login page.
type LoginGate struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// APILevel is the version of the server API the Web UI is written against.
// The server reports it (in /healthz and in <prefix>/login-config.json), the
// embedded UI is built for it, and a UI bundle loaded from disk declares the
// range of levels it works with in its manifest, so a bundle built for another
// API is refused instead of half-working.
//
// Bump it when a route, field or behaviour the Web UI relies on changes
// incompatibly: a route removed or renamed, a response field removed, renamed or
// retyped, a request field made required, or the meaning of a value changed.
// Never bump it for an additive change — a new route, a new optional field, a
// new enum value the UI can ignore — because every existing bundle would then be
// refused for no reason.
//
// Level 2 (RFC FA P0): a Web UI session cookie no longer carries the bearer,
// and a state-changing request authenticated by it must send
// `X-Loomcycle-UI: 1`; the login route creates a session and logout is
// `POST /v1/_auth/logout`. A level-1 bundle sends no header, so every write it
// made would be refused.
const APILevel = 2

// uiAssets embeds the production build of the web/ React app.
//
//go:embed all:dist
var uiAssets embed.FS

// Handler returns the http.Handler that serves the embedded SPA at
// the given prefix (typically "/ui"). The handler:
//
//   - GET <prefix>             serves index.html. A `?token=` query, the
//     pre-session way in, is no longer honoured: it redirects to the login
//     page so the token drops out of the address bar, and sets nothing.
//   - GET <prefix>/login-config.json  the gates the login page offers.
//   - GET <prefix>/<file>      serves embedded asset OR falls back
//     to index.html (SPA-router pattern). <prefix>/logout is such a deep
//     link: the SPA's page there only asks, and its button posts to the
//     server's logout route, so neither the GET nor loading the page ends a
//     session.
//
// login is asked per request, because whether a session can be started
// depends on the transport the request came over.
//
// The SPA files come from the embedded build unless WithBundle hands it a
// verified bundle loaded from disk. Either way the routes above are the same
// Go handlers, never read from the SPA files.
func Handler(prefix string, login func(*http.Request) LoginConfig, opts ...Option) http.Handler {
	prefix = strings.TrimRight(prefix, "/")
	mux := http.NewServeMux()
	var o handlerOptions
	for _, opt := range opts {
		opt(&o)
	}
	var src source = embeddedSource{}
	if o.bundle != nil {
		src = o.bundle
	}

	mux.HandleFunc(fmt.Sprintf("GET %s", prefix), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("token") {
			http.Redirect(w, r, prefix+"/login", http.StatusFound)
			return
		}
		serveFile(w, src, "index.html")
	})

	// GET <prefix>/login-config.json — what the login page needs before anyone
	// signs in: the mode, the gate names and labels, and the origins the
	// postMessage handoff may accept a bearer from (LOOMCYCLE_UI_LOGIN_ORIGINS;
	// empty ⇒ the receiver stays disabled). Never secret; served
	// unauthenticated.
	mux.HandleFunc(fmt.Sprintf("GET %s/login-config.json", prefix), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var lc LoginConfig
		if login != nil {
			lc = login(r)
		}
		if lc.Origins == nil {
			lc.Origins = []string{}
		}
		if lc.Gates == nil {
			lc.Gates = []LoginGate{}
		}
		body := map[string]any{
			"login_origins": lc.Origins,
			"api_level":     APILevel,
			"mode":          lc.Mode,
			"gates":         lc.Gates,
		}
		if lc.SessionRefused != "" {
			body["session_refused"] = lc.SessionRefused
		}
		if lc.App != nil {
			body["app"] = lc.App
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	mux.HandleFunc(fmt.Sprintf("GET %s/", prefix), func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, prefix+"/")
		if rel == "" {
			serveFile(w, src, "index.html")
			return
		}
		// Path traversal guard. embed.FS would resolve safely, but
		// rejecting at the boundary keeps the audit story clean.
		if strings.Contains(rel, "..") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		// Try the file; on miss, fall back to index.html so React
		// Router takes over (deep links to /ui/agents/{id}).
		if data, ok := src.file(rel); ok {
			writeFile(w, rel, data)
			return
		}
		serveFile(w, src, "index.html")
	})

	return mux
}

// source is where the handler reads SPA files from. Every source is served by
// the same handler, so the routes and response headers cannot differ between
// them.
type source interface {
	// file returns the bytes of the named regular file (slash-separated,
	// relative to the SPA root); ok is false when there is none.
	file(name string) (data []byte, ok bool)
}

// embeddedSource serves the SPA compiled into the binary.
type embeddedSource struct{}

func (embeddedSource) file(name string) ([]byte, bool) {
	// ReadFile refuses a directory, so a directory name is a miss.
	data, err := fs.ReadFile(uiAssets, "dist/"+name)
	return data, err == nil
}

// serveFile writes the named file from src. Returns 503 with a diagnostic
// code when the file is absent — for index.html that means dist/ is empty
// (UI not built).
func serveFile(w http.ResponseWriter, src source, name string) {
	data, ok := src.file(name)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"code":"ui_not_built","error":"web UI not built; run \"make build-ui\" (or \"cd web && npm install && npm run build\") before starting loomcycle"}`)
		return
	}
	writeFile(w, name, data)
}

// writeFile writes a file's bytes with the content-type and caching rules
// every source shares.
func writeFile(w http.ResponseWriter, name string, data []byte) {
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
