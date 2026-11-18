package http

import (
	"net/http/httptest"
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
	"GET /v1/openapi.json": true, "GET /v1/openapi.yaml": true, "GET /v1/docs": true, "GET /v1/docs/{file}": true,
	"GET /ui": true, "GET /ui/": true, "POST /ui/session": true,
	// Signing out ends the caller's own session and runs before any
	// principal exists (an expired session may still log out).
	"POST /v1/_auth/logout": true,
	// The caller's own app sign-ins: listing them is whoami's kind of
	// question and ending one is signing out of an app, both about the
	// caller only (the operator twins under /v1/_users ask principal:user).
	"GET /v1/_me/app-sign-ins": true, "DELETE /v1/_me/app-sign-ins/{family_id}": true,
	// The password gate's login, invite and reset posts establish a session
	// (no principal yet); the account routes act on the caller's own
	// credentials only.
	"POST /v1/_auth/complete/{gate}": true, "POST /v1/_auth/invite/info": true, "POST /v1/_auth/invite": true,
	"POST /v1/_auth/reset/info": true, "POST /v1/_auth/reset": true, "GET /v1/_auth/account": true,
	"POST /v1/_auth/password": true, "POST /v1/_auth/totp/begin": true, "POST /v1/_auth/totp/confirm": true, "DELETE /v1/_auth/totp": true,
	// RFC FA P6 passkeys: the sign-in start precedes any principal (the
	// completion is POST /v1/_auth/complete/{gate} above); the management
	// routes act on the caller's own passkeys only, from its own Web UI
	// session, like the password and TOTP routes.
	"POST /v1/_auth/passkey/{gate}/begin": true, "GET /v1/_auth/passkeys": true,
	"POST /v1/_auth/passkeys/register/begin": true, "POST /v1/_auth/passkeys/register/finish": true,
	"PATCH /v1/_auth/passkeys/{id}": true, "DELETE /v1/_auth/passkeys/{id}": true,
	// The redirect gates' login start and provider callback establish a
	// session (no principal yet).
	"GET /v1/_auth/login/{gate}": true, "GET /v1/_auth/complete/{gate}": true,
	// RFC FA P4: an external gate's assert, revoke and sync carry no bearer —
	// the gate's signature under its pinned key authenticates them, and what
	// they do is held to the gate's identity_map.
	"POST /v1/_auth/assert": true, "POST /v1/_auth/revoke": true, "POST /v1/_auth/sync": true,
	// RFC FA P5 device login: the CLI's start and poll precede any principal
	// (the poll authenticates by its device code; the approval is governed).
	"POST /v1/_auth/device": true, "POST /v1/_auth/device/token": true,
	"GET /.well-known/agent-card.json/extended": true,
	// RFC FL: the app sign-in routes. authorize runs on the person's own
	// session; token and revoke are authenticated by the code (with its PKCE
	// verifier) or the refresh token; the metadata is public.
	"GET /v1/_oauth/authorize": true, "POST /v1/_oauth/token": true, "OPTIONS /v1/_oauth/token": true,
	"POST /v1/_oauth/revoke": true, "OPTIONS /v1/_oauth/revoke": true, "GET /.well-known/oauth-authorization-server": true,
	// The evaluate / explain / simulate questions are answered FOR a subject
	// and confined to it by the handler; they are not themselves governed.
	"POST /v1/_authz/evaluation": true, "POST /v1/_authz/evaluations": true, "POST /v1/_authz/explain": true, "POST /v1/_authz/simulate": true,
	// RFC FK P3: the voice controls' info. The route admits any runs:create
	// caller; the handler asks each audio route's own question and shows a
	// direction only to a caller who may use it.
	"GET /v1/_speech": true,
}

var muxPatternRe = regexp.MustCompile(`mux\.(?:Handle|HandleFunc)\("([A-Z]+ )?([^"]+)"`)

// RFC EZ P6: every route the server registers has exactly one row in the
// route table, under the pattern it is registered with, and the table holds
// no row for a route nobody registers. The table is what the scope, run-plane
// and ACL gates read, so a route added without a row would be gated as no
// route at all.
func TestACLRoutes_EveryRegisteredRouteHasOneRow(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range aclRegisteredPatterns(t) {
		registered[p] = true
	}
	if len(registered) < 250 {
		t.Fatalf("only %d registered patterns found; the census no longer reads the route files", len(registered))
	}
	rows := map[string]bool{}
	for _, rt := range authz.HTTPRoutes() {
		if rows[rt.Pattern] {
			t.Errorf("%s has two rows", rt.Pattern)
		}
		rows[rt.Pattern] = true
		if !registered[rt.Pattern] {
			t.Errorf("row %s names a route nothing registers", rt.Pattern)
		}
	}
	for p := range registered {
		if !rows[p] {
			t.Errorf("%s is registered but has no row in authz's route table", p)
		}
	}
}

// A route no ACL governs is one of aclUngovernedRoutes, each listed with why,
// and every listed route is ungoverned: a row cannot drop its question
// without that list saying so. Every governed row asks a catalog pair.
func TestACLSurfaces_EveryRegisteredRouteIsGovernedOrListed(t *testing.T) {
	listed := map[string]bool{}
	for _, rt := range authz.HTTPRoutes() {
		key := rt.Pattern
		if !strings.Contains(key, " ") {
			key = "GET " + key // a method-less pattern serves every method; GET is the weakest question
		}
		method, path, _ := strings.Cut(key, " ")
		if !rt.Governed() {
			listed[key] = true
			if !aclUngovernedRoutes[key] {
				t.Errorf("%s has no ACL question; give its row one or list it in aclUngovernedRoutes", rt.Pattern)
			}
			continue
		}
		sample := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "x")
		s, _ := rt.Question(method, sample)
		if !authz.ValidVerb(s.Type, s.Action) {
			t.Errorf("%s → %s %s is not a catalog pair", rt.Pattern, s.Action, s.Type)
		}
	}
	for key := range aclUngovernedRoutes {
		if !listed[key] {
			t.Errorf("%s is listed as ungoverned but its row asks a question (or it has no row)", key)
		}
	}
}
