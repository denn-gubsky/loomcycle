package http

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// httptestServerAt wraps a URL for bearerCall, which takes the test server
// for its URL only.
func httptestServerAt(url string) *httptest.Server { return &httptest.Server{URL: url} }

// aclUngovernedRoutes are the registered routes no ACL governs: the health
// and docs endpoints, whoami, the public config view, and the UI shell.
// Everything else the mux registers must map to a catalog question
// (authz.HTTPSurface), so a route added later cannot land ungoverned by
// accident (RFC EZ §2).
var aclUngovernedRoutes = map[string]bool{
	"GET /healthz": true, "GET /v1/_me": true, "GET /v1/config": true,
	"GET /v1/openapi.json": true, "GET /v1/openapi.yaml": true, "GET /v1/docs": true, "GET /v1/docs/": true, "GET /v1/docs/{file}": true,
	"GET /ui": true, "GET /ui/": true, "POST /ui/session": true, "GET /ui/login-config.json": true, "GET /ui/logout": true, "POST /ui/logout": true,
	// Signing out ends the caller's own session and runs before any
	// principal exists (an expired session may still log out).
	"POST /v1/_auth/logout": true,
	// The password gate's login, invite and reset posts establish a session
	// (no principal yet); the account routes act on the caller's own
	// credentials only.
	"POST /v1/_auth/complete/{gate}": true, "POST /v1/_auth/invite/info": true, "POST /v1/_auth/invite": true,
	"POST /v1/_auth/reset/info": true, "POST /v1/_auth/reset": true, "GET /v1/_auth/account": true,
	"POST /v1/_auth/password": true, "POST /v1/_auth/totp/begin": true, "POST /v1/_auth/totp/confirm": true, "DELETE /v1/_auth/totp": true,
	"GET /.well-known/agent-card.json/extended": true,
	// The evaluate / explain / simulate questions are answered FOR a subject
	// and confined to it by the handler; they are not themselves governed.
	"POST /v1/_authz/evaluation": true, "POST /v1/_authz/evaluations": true, "POST /v1/_authz/explain": true, "POST /v1/_authz/simulate": true,
}

var muxPatternRe = regexp.MustCompile(`mux\.(?:Handle|HandleFunc)\("([A-Z]+ )?([^"]+)"`)

func TestACLSurfaces_EveryRegisteredRouteIsGovernedOrListed(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, m := range muxPatternRe.FindAllStringSubmatch(string(src), -1) {
		method := strings.TrimSpace(m[1])
		pattern := m[2]
		if method == "" {
			method = "GET" // a method-less pattern serves every method; GET is the weakest question
		}
		key := method + " " + pattern
		if aclUngovernedRoutes[key] {
			continue
		}
		// Placeholders become a plausible segment; `{path...}` becomes one
		// segment too.
		path := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(pattern, "x")
		path = strings.TrimSuffix(path, "/")
		if path == "" {
			path = "/"
		}
		seen++
		s, ok := authz.HTTPSurface(method, path)
		if !ok {
			t.Errorf("%s %s (as %s) is registered but no ACL question governs it; map it in authz.HTTPSurface or list it in aclUngovernedRoutes", method, pattern, path)
			continue
		}
		if !authz.ValidVerb(s.Type, s.Action) {
			t.Errorf("%s %s → %s %s is not a catalog pair", method, pattern, s.Action, s.Type)
		}
	}
	if seen < 150 {
		t.Fatalf("only %d routes seen; the census regexp no longer matches server.go", seen)
	}
}
