package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The self-service decision for the identity routes is made after routing,
// from the matched pattern and path value: an encoded '/' that makes the
// decoded path look like "<self>/identities…" while the mux routes it to
// another route (here DELETE /v1/_users/{subject}) must not pass the scope
// gate as self-service.
func TestIdentityRoutes_EncodedSlashCannotPassAsSelfService(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeCompat)
	ctx := context.Background()
	// The member bearer's subject is "dev"; "dev/identities/g" is a user it
	// must not be able to delete.
	for _, sub := range []string{"dev/identities/g", "dev/identities"} {
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptestServerAt(url)
	for _, c := range []struct{ method, path string }{
		{"DELETE", "/v1/_users/dev%2Fidentities%2Fg"},
		{"GET", "/v1/_users/dev%2Fidentities"},
		{"PATCH", "/v1/_users/dev%2Fidentities"},
	} {
		code, body := bearerCall(t, srv, freeBearer, c.method, c.path, "{}")
		if code != http.StatusForbidden {
			t.Errorf("%s %s = %d %s, want the scope gate's 403", c.method, c.path, code, body)
		}
	}
	for _, sub := range []string{"dev/identities/g", "dev/identities"} {
		if _, err := st.UserGet(ctx, "acme", sub); err != nil {
			t.Errorf("user %q was removed: %v", sub, err)
		}
	}
	// The real self path still works for the member's own subject.
	if code, body := bearerCall(t, srv, freeBearer, "GET", "/v1/_users/dev/identities", ""); code == http.StatusForbidden {
		t.Errorf("own identities refused: %s", body)
	}
}
