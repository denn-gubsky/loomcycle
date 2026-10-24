package http

import (
	"context"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// authManageWant is every route an auth:manage credential reaches (RFC FA
// D16): the user roster's list / create / update / delete and the password
// gate's management API. A route added to it must be added here by name.
var authManageWant = map[string]bool{
	"GET /v1/_users":              true,
	"POST /v1/_users":             true,
	"PATCH /v1/_users/{subject}":  true,
	"DELETE /v1/_users/{subject}": true,

	"POST /v1/_authgate/{gate}/invites":                 true,
	"GET /v1/_authgate/{gate}/invites":                  true,
	"DELETE /v1/_authgate/{gate}/invites/{id}":          true,
	"POST /v1/_authgate/{gate}/resets":                  true,
	"DELETE /v1/_authgate/{gate}/credentials/{subject}": true,
	"PATCH /v1/_authgate/{gate}/logins/{subject}":       true,
	"GET /v1/_authgate/{gate}/identities":               true,
	"GET /v1/_authgate/{gate}/tenant-status":            true,
}

// The route census (RFC FA security row "auth:manage refused on every route
// outside its list"): through the real authMiddleware, the credential is
// admitted on exactly the listed routes and refused on every other route the
// mux registers — runs, data, whoami, token minting, groups and ACLs
// included.
func TestAuthManage_ReachesExactlyItsRouteList(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_site", Name: "site", TenantID: "acme", Subject: "site",
		TokenHash: auth.HashToken(testTokenPepper, "site-bearer"), AllowedScopes: []string{auth.ScopeAuthManage},
	}); err != nil {
		t.Fatal(err)
	}
	var got, missing []string
	for _, r := range eqRoutes(t) {
		key := r.method + " " + r.pattern
		reached, code := runMW(t, s, r.method, r.path, "site-bearer")
		if reached && !authManageWant[key] {
			got = append(got, key)
		}
		if !reached && authManageWant[key] {
			missing = append(missing, key)
		}
		if !reached && code != 403 {
			t.Errorf("%s: refused with %d, want 403", key, code)
		}
	}
	sort.Strings(got)
	sort.Strings(missing)
	if len(got) > 0 {
		t.Errorf("auth:manage reaches routes outside its list: %v", got)
	}
	if len(missing) > 0 {
		t.Errorf("auth:manage is refused routes on its list: %v", missing)
	}
}
