package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The Web UI files that name the pages /v1/_me's capability summary
// answers for. String literals starting "../" so the CI test selector runs
// this package when one of them changes.
const (
	navItemsTS         = "../../../web/src/components/navItems.ts"
	settingsSectionsTS = "../../../web/src/lib/settingsSections.ts"
	adapterTypesTS     = "../../../adapters/ts/src/types.ts"
)

// navClass is one principal class the capability summary is asked for.
type navClass struct {
	name, bearer, subject string
}

// navFixture is the restricted-token fixture (a member token "dev") with
// the ACL resolver wired, plus a tenant operator, a restricted operator, an
// isolated user and an admin token in the same tenant.
func navFixture(t *testing.T, mode authz.Mode) (*Server, string, store.Store, []navClass) {
	t.Helper()
	srv, url, st := aclFixture(t, mode)
	for _, tok := range []struct {
		id, bearer, subject string
		scopes              []string
	}{
		{"def_op", "tok-nav-op", "op", []string{auth.ScopeTenant}},
		{"def_op2", "tok-nav-op2", "op2", []string{auth.ScopeTenant}},
		{"def_iso", "tok-nav-iso", "iso", []string{auth.ScopeUser}},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: tok.id, Name: "u-" + tok.id, TenantID: "acme", Subject: tok.subject,
			TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: tok.scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	classes := []navClass{
		{"member", freeBearer, "dev"},
		{"tenant operator", "tok-nav-op", "op"},
		{"restricted operator", "tok-nav-op2", "op2"},
		{"isolated", "tok-nav-iso", "iso"},
		{"legacy", "legacy-admin-secret", ""},
	}
	return srv, url, st, classes
}

// navSurfacesOf reads effective.surfaces off GET /v1/_me.
func navSurfacesOf(t *testing.T, url, bearer string) map[string]bool {
	t.Helper()
	code, body := bearerCall(t, httptestServerAt(url), bearer, "GET", "/v1/_me", "")
	if code != 200 {
		t.Fatalf("GET /v1/_me: %d %s", code, body)
	}
	var me struct {
		Effective *struct {
			Surfaces map[string]bool `json:"surfaces"`
		} `json:"effective"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatal(err)
	}
	if me.Effective == nil || me.Effective.Surfaces == nil {
		t.Fatalf("GET /v1/_me carries no effective.surfaces: %s", body)
	}
	return me.Effective.Surfaces
}

// gateAdmits is the independent answer: does the request to the surface's
// primary route get past authMiddleware (every route gate: scope, member
// path, self-service, run plane, the ACL type-level question)? It drives
// the real middleware in front of a sentinel handler, so it cannot share a
// bug with the summary's own evaluation.
func gateAdmits(t *testing.T, srv *Server, bearer string, ns navSurface, subject string) bool {
	t.Helper()
	reached := false
	h := srv.authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	req := httptest.NewRequest(ns.Method, strings.ReplaceAll(ns.Path, "{subject}", subject), strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+bearer)
	h.ServeHTTP(httptest.NewRecorder(), req)
	return reached
}

func operatorHeld(srv *Server, bearer string) bool {
	p, ok := srv.resolvePrincipal(context.Background(), bearer)
	return ok && (p.Legacy || auth.HasScope(p.Scopes, auth.ScopeTenant))
}

// For every principal class and every page, the summary says exactly what
// a request to the page's primary route would be told by the route gates
// (and, for a page that administers the tenant, by the handler's
// operator-only check). Each class runs with an ACL bound to it as well as
// without, so the ACL half of the answer is exercised, not only the scopes.
func TestWhoamiEffective_MatchesTheRouteGatesForEveryClass(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			srv, url, st, classes := navFixture(t, mode)
			putACL(t, st, "no-channels", `[{"effect":"deny","actions":["subscribe"],"resources":["channel:*"]}]`)
			putACL(t, st, "no-acl-reads", `[{"effect":"deny","actions":["read"],"resources":["def:acl/*"]}]`)
			bind(t, st, "no-channels", "user", "dev")
			bind(t, st, "no-acl-reads", "user", "op2")
			// The substrate:admin class last: minting it retires the legacy secret.
			for i, c := range append(classes, navClass{"admin", "tok-nav-root", "root"}) {
				if i == len(classes) {
					addRootToken(t, st)
				}
				got := navSurfacesOf(t, url, c.bearer)
				if len(got) != len(navSurfaces) {
					t.Errorf("%s: %d surfaces, the table has %d", c.name, len(got), len(navSurfaces))
				}
				op := operatorHeld(srv, c.bearer)
				for _, ns := range navSurfaces {
					want := gateAdmits(t, srv, c.bearer, ns, c.subject) && (!ns.Operator || op)
					if got[ns.ID] != want {
						t.Errorf("%s: %s (%s %s) = %v, the route gates say %v", c.name, ns.ID, ns.Method, ns.Path, got[ns.ID], want)
					}
				}
			}
		})
	}
}

// The cells the parity test above does not pin by itself: that the answers
// differ by class at all, and the four cases the navigation work is for.
func TestWhoamiEffective_PinsTheClassDifferences(t *testing.T) {
	_, url, st, _ := navFixture(t, authz.ModeCompat)
	putACL(t, st, "no-channels", `[{"effect":"deny","actions":["subscribe"],"resources":["channel:*"]}]`)
	putACL(t, st, "no-acl-reads", `[{"effect":"deny","actions":["read"],"resources":["def:acl/*"]}]`)

	member := navSurfacesOf(t, url, freeBearer)
	for id, want := range map[string]bool{
		"run": true, "runs": true, "my-credentials": true, "channels": true, "library": true,
		"snapshots": false, "activity": false, "users": false, "credentials": false, "tokens": false,
		"groups": false, "policies": false, "bindings": false, "simulator": false,
	} {
		if member[id] != want {
			t.Errorf("member without an ACL: %s = %v, want %v", id, member[id], want)
		}
	}
	bind(t, st, "no-channels", "user", "dev")
	if navSurfacesOf(t, url, freeBearer)["channels"] {
		t.Error("member bound to a deny on subscribe channel:* still reaches channels")
	}

	op := navSurfacesOf(t, url, "tok-nav-op")
	if !op["policies"] || !op["groups"] || !op["bindings"] || !op["simulator"] || !op["users"] || !op["credentials"] || op["tokens"] || op["snapshots"] {
		t.Errorf("tenant operator: %v", op)
	}
	bind(t, st, "no-acl-reads", "user", "op2")
	op2 := navSurfacesOf(t, url, "tok-nav-op2")
	if op2["policies"] || !op2["users"] {
		t.Errorf("operator denied read def:acl/*: policies=%v users=%v", op2["policies"], op2["users"])
	}

	iso := navSurfacesOf(t, url, "tok-nav-iso")
	if !iso["run"] || !iso["my-credentials"] || iso["library"] || iso["channels"] || iso["credentials"] {
		t.Errorf("isolated user: %v", iso)
	}

	// The legacy secret first: minting a substrate:admin token retires it.
	for _, bearer := range []string{"legacy-admin-secret", "tok-nav-root"} {
		if bearer == "tok-nav-root" {
			addRootToken(t, st)
		}
		for id, ok := range navSurfacesOf(t, url, bearer) {
			if !ok {
				t.Errorf("admin %s: %s = false", bearer, id)
			}
		}
	}
}

// addRootToken mints a substrate:admin token "tok-nav-root". Once one
// exists the legacy secret stops authenticating (the no-lockout migration
// gate), so a test asks the legacy class first.
func addRootToken(t *testing.T, st store.Store) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_root", Name: "u-def_root", TenantID: "acme", Subject: "root",
		TokenHash: auth.HashToken(testTokenPepper, "tok-nav-root"), AllowedScopes: []string{auth.ScopeAdmin},
	}); err != nil {
		t.Fatal(err)
	}
}

// Open mode (no auth configured) reaches every page.
func TestWhoamiEffective_OpenModeReachesEverything(t *testing.T) {
	srv := New(makeBaseConfig(), &stubResolver{}, nil, concurrency.New(8, 8, 5*time.Second), nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/_me", nil))
	var me struct {
		OpenMode  bool `json:"open_mode"`
		Effective struct {
			Surfaces map[string]bool `json:"surfaces"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || !me.OpenMode {
		t.Fatalf("open-mode whoami: %v %s", err, rec.Body.String())
	}
	if len(me.Effective.Surfaces) != len(navSurfaces) {
		t.Fatalf("open mode: %d surfaces, want %d", len(me.Effective.Surfaces), len(navSurfaces))
	}
	for id, ok := range me.Effective.Surfaces {
		if !ok {
			t.Errorf("open mode: %s = false", id)
		}
	}
}

// Every table entry names a route the mux serves, by its method, and a
// distinct page id.
func TestNavSurfaces_EveryEntryRouteIsRegistered(t *testing.T) {
	srv, _, _, _ := navFixture(t, authz.ModeCompat)
	mux, ok := srv.Mux().(*http.ServeMux)
	if !ok {
		t.Fatal("Mux() is no longer a *http.ServeMux")
	}
	seen := map[string]bool{}
	for _, ns := range navSurfaces {
		if seen[ns.ID] {
			t.Errorf("%s appears twice", ns.ID)
		}
		seen[ns.ID] = true
		req := httptest.NewRequest(ns.Method, strings.ReplaceAll(ns.Path, "{subject}", "dev"), nil)
		_, pattern := mux.Handler(req)
		if pattern == "" || !strings.HasPrefix(pattern, ns.Method+" ") {
			t.Errorf("%s: %s %s is served by %q, not a route of its own method", ns.ID, ns.Method, ns.Path, pattern)
		}
		if _, governed := authz.HTTPSurface(ns.Method, ns.Path); !governed {
			t.Errorf("%s: %s %s asks no ACL question, so the summary could not reflect an ACL", ns.ID, ns.Method, ns.Path)
		}
	}
}

var (
	tsIDRe       = regexp.MustCompile(`\bid:\s*"([a-z0-9-]+)"`)
	tsNavBlockRe = regexp.MustCompile(`(?s)export const NAV_ITEMS[^=]*=\s*\[(.*?)\n\];`)
	tsSecBlockRe = regexp.MustCompile(`(?s)export const SECTIONS[^=]*=\s*\[(.*?)\n\];`)
	tsLinkRe     = regexp.MustCompile(`(?s)export const ACCOUNT_LINKS[^=]*=\s*\[(.*?)\n\];`)
)

func tsIDs(t *testing.T, file string, block *regexp.Regexp) []string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m := block.FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s: the list this test reads is no longer where it was", file)
	}
	var ids []string
	for _, id := range tsIDRe.FindAllSubmatch(m[1], -1) {
		ids = append(ids, string(id[1]))
	}
	return ids
}

// The table and the Web UI's page lists name the same pages: every left-nav
// item, Settings section and settings-menu link has an entry, and every
// entry is one of them. A page with no entry would fall back to the role
// rule; an entry with no page is a capability nobody reads.
func TestNavSurfaces_CoverEveryWebUIPage(t *testing.T) {
	var ui []string
	ui = append(ui, tsIDs(t, navItemsTS, tsNavBlockRe)...)
	ui = append(ui, tsIDs(t, settingsSectionsTS, tsSecBlockRe)...)
	ui = append(ui, tsIDs(t, settingsSectionsTS, tsLinkRe)...)
	if len(ui) < 25 {
		t.Fatalf("read only %d page ids from the Web UI; the parse is broken: %v", len(ui), ui)
	}
	var table []string
	for _, ns := range navSurfaces {
		table = append(table, ns.ID)
	}
	sort.Strings(ui)
	sort.Strings(table)
	if strings.Join(ui, ",") != strings.Join(table, ",") {
		t.Errorf("Web UI pages and the capability table differ:\n  ui:    %v\n  table: %v", ui, table)
	}
}

// The TS adapter's WhoamiResponse declares the capability summary.
func TestWhoamiEffective_DeclaredByTheTSAdapter(t *testing.T) {
	src, err := os.ReadFile(adapterTypesTS)
	if err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`(?s)export interface WhoamiResponse \{(.*?)\n\}`).FindSubmatch(src)
	if body == nil || !regexp.MustCompile(`\n\s*effective\?: WhoamiEffective;`).Match(body[1]) {
		t.Fatal("WhoamiResponse has no `effective?: WhoamiEffective`")
	}
	eff := regexp.MustCompile(`(?s)export interface WhoamiEffective \{(.*?)\n\}`).FindSubmatch(src)
	if eff == nil || !regexp.MustCompile(`\n\s*surfaces: Record<string, boolean>;`).Match(eff[1]) {
		t.Fatal("WhoamiEffective has no `surfaces: Record<string, boolean>`")
	}
}
