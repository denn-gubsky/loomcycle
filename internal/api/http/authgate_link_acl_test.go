package http

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A tenant operator's ACL governs another user's identities like the rest
// of the user plane: an operator denied `manage`/`read principal:user:victim`
// can neither list nor unlink (nor approve) the victim, while a user it is not
// denied stays reachable.
func TestIdentityRoutes_OperatorIsHeldToItsACL(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeCompat)
	ctx := context.Background()
	const opBearer = "lct_identity_acl_operator"
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: "def_op", Name: "op", TenantID: "acme",
		Subject: "op", TokenHash: auth.HashToken(testTokenPepper, opBearer), AllowedScopes: []string{auth.ScopeTenant}}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"victim", "other"} {
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: store.UserStatusPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: "acme", Subject: sub, Gate: "idp", Issuer: "i", ExternalSubject: sub}); err != nil {
			t.Fatal(err)
		}
	}
	putACL(t, st, "hands-off", `[{"effect":"deny","actions":["read","manage"],"resources":["principal:user:victim"]}]`)
	bind(t, st, "hands-off", "user", "op")
	srv := httptestServerAt(url)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/_users/victim/identities"},
		{"DELETE", "/v1/_users/victim/identities/idp"},
		{"POST", "/v1/_users/victim/approve"},
	} {
		code, body := bearerCall(t, srv, opBearer, c.method, c.path, "")
		if code != http.StatusForbidden || !strings.Contains(body, "acl_denied") {
			t.Errorf("%s %s = %d %s", c.method, c.path, code, body)
		}
	}
	if _, err := st.UserIdentityGet(ctx, "idp", "i", "victim"); err != nil {
		t.Fatal("the victim's identity was removed")
	}
	if code, body := bearerCall(t, srv, opBearer, "GET", "/v1/_users/other/identities", ""); code != http.StatusOK {
		t.Errorf("an undenied user = %d %s", code, body)
	}
}
