package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Disabling a user ends its app sign-ins, and a re-enable does not revive
// them (RFC FY Part F, audit A3).

const appOpBearer = "acme-op-erasure-bearer"

// appOpServer serves the fixture's mux to an acme tenant operator.
func appOpServer(t *testing.T, f *appFixture) *httptest.Server {
	t.Helper()
	seedToken(t, f.st, appOpBearer, "acme", "acme-op", []string{auth.ScopeTenant}, time.Time{})
	ts := httptest.NewServer(f.s.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func TestDisable_RevokesRefreshFamiliesAndReEnableDoesNotReviveThem(t *testing.T) {
	disableThenEnable := map[string]func(t *testing.T, f *appFixture, ts *httptest.Server){
		"operator patch": func(t *testing.T, f *appFixture, ts *httptest.Server) {
			for _, st := range []string{"disabled", "active"} {
				if code, body := bearerCall(t, ts, appOpBearer, "PATCH", "/v1/_users/google:100", `{"status":"`+st+`"}`); code != http.StatusOK {
					t.Fatalf("PATCH status %s = %d %s", st, code, body)
				}
			}
		},
		"removal policy": func(t *testing.T, f *appFixture, _ *httptest.Server) {
			ctx := context.Background()
			if err := f.s.startRemoval(ctx, "acme", "google:100", "", store.RemovalReasonDormant, "test"); err != nil {
				t.Fatal(err)
			}
			if err := f.s.reactivateRemoved(ctx, "acme", "google:100", "test"); err != nil {
				t.Fatal(err)
			}
			// A re-enable drops the user to the floor's access_mode, which
			// alone refuses the app's tenant-scoped refresh. Put it back, so
			// what is asserted is the family's revocation, not the floor.
			mode := "tenant"
			if err := f.st.UserUpdate(ctx, "acme", "google:100", store.UserPatch{AccessMode: &mode}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, run := range disableThenEnable {
		t.Run(name, func(t *testing.T) {
			f := newAppFixture(t, "tenant")
			ts := appOpServer(t, f)
			_, rt := f.signIn(t)
			run(t, f, ts)
			if u, _ := f.st.UserGet(context.Background(), "acme", "google:100"); u.Status != "active" {
				t.Fatalf("precondition: the user is %q after the re-enable", u.Status)
			}
			if rec, body := f.refresh(t, rt); rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
				t.Fatalf("refresh after disable and re-enable = %d %v, want 400 invalid_grant", rec.Code, body)
			}
		})
	}
}

// A SCIM deprovision is a disable too: the user's app sign-ins end with it,
// and the SCIM re-enable does not bring them back.
func TestSCIMDeprovision_RevokesTheUsersAppSignIns(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	ctx := context.Background()
	id, subject := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	now := time.Now().UTC()
	if err := f.st.OAuthFamilyCreate(ctx, store.OAuthFamilyRow{FamilyID: "fam-scim", ClientID: "board", TenantID: "acme",
		Subject: subject, Gate: "okta", CreatedAt: now, LastUsedAt: now,
		IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	off := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": false})
	on := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": true})
	for _, body := range []string{off, on} {
		if code, out := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, body); code != http.StatusOK {
			t.Fatalf("SCIM PATCH = %d %v", code, out)
		}
	}
	if f.status(t, "acme", subject) != "active" {
		t.Fatal("precondition: SCIM did not re-enable the user")
	}
	fam, err := f.st.OAuthFamilyGet(ctx, "fam-scim")
	if err != nil || fam.RevokedAt.IsZero() {
		t.Fatalf("the deprovisioned user's app sign-in survived: %+v, %v", fam, err)
	}
}
