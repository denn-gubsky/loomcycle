package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// mintRaceStore holds a refresh at its access-token insert — after the
// refresh found the user active — until the disable under test has retired
// the user's tokens (its UISessionRevokeSubject), then lets the refresh
// finish before the disable goes on. That is the interleaving where a token
// minted from a still-live family lands after the token retirement.
type mintRaceStore struct {
	store.Store
	armed    atomic.Bool
	minting  chan struct{} // closed when the refresh reaches its insert
	release  chan struct{} // closed to let it insert
	refreshd chan struct{} // closed when the refresh has answered
	once     sync.Once
}

func (w *mintRaceStore) OperatorTokenDefCreate(ctx context.Context, row store.OperatorTokenDefRow) (store.OperatorTokenDefRow, error) {
	if w.armed.CompareAndSwap(true, false) {
		close(w.minting)
		<-w.release
	}
	return w.Store.OperatorTokenDefCreate(ctx, row)
}

func (w *mintRaceStore) UISessionRevokeSubject(ctx context.Context, tenantID, subject string, at time.Time) (int, error) {
	w.once.Do(func() {
		close(w.release)
		<-w.refreshd
	})
	return w.Store.UISessionRevokeSubject(ctx, tenantID, subject, at)
}

// TestDisable_AnAccessTokenMintedDuringTheDisableDoesNotAuthenticate: a
// refresh racing the disable must not leave an access token that outlives
// it — after the disable, and after a re-enable too.
func TestDisable_AnAccessTokenMintedDuringTheDisableDoesNotAuthenticate(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ts := appOpServer(t, f)
	_, rt := f.signIn(t)
	w := &mintRaceStore{Store: f.st, minting: make(chan struct{}), release: make(chan struct{}), refreshd: make(chan struct{})}
	w.armed.Store(true)
	f.s.store = w

	var raced string
	go func() {
		defer close(w.refreshd)
		if rec, body := f.refresh(t, rt); rec.Code == http.StatusOK {
			raced, _ = body["access_token"].(string)
		}
	}()
	<-w.minting
	if code, body := bearerCall(t, ts, appOpBearer, "PATCH", "/v1/_users/google:100", `{"status":"disabled"}`); code != http.StatusOK {
		t.Fatalf("disable = %d %s", code, body)
	}
	if raced == "" {
		return // the refresh was refused: nothing outlives the disable
	}
	if c, _ := f.bearerMe(t, raced); c == http.StatusOK {
		t.Fatal("an access token minted during the disable authenticates after it")
	}
	if code, body := bearerCall(t, ts, appOpBearer, "PATCH", "/v1/_users/google:100", `{"status":"active"}`); code != http.StatusOK {
		t.Fatalf("re-enable = %d %s", code, body)
	}
	if c, _ := f.bearerMe(t, raced); c == http.StatusOK {
		t.Fatal("an access token minted during the disable authenticates again after the re-enable")
	}
}

// TestDisable_ARetriedDisableRetiresTheTokensOfAlreadyRevokedSignIns: a
// disable that revoked the user's app sign-ins and failed before retiring
// their access tokens is finished by the retry, although the retry finds no
// family left to revoke.
func TestDisable_ARetriedDisableRetiresTheTokensOfAlreadyRevokedSignIns(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ctx := context.Background()
	at, _ := f.signIn(t)
	p, ok := f.s.resolvePrincipal(ctx, at)
	if !ok {
		t.Fatal("precondition: the app's access token does not authenticate")
	}
	// The first attempt's partial effect.
	if _, err := f.st.OAuthFamilyRevokeSubject(ctx, "acme", "google:100", oauthRevokeUserDisabled, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.endSubjectAppSignIns(ctx, "acme", "google:100"); err != nil {
		t.Fatal(err)
	}
	row, err := f.st.OperatorTokenDefGet(ctx, p.TokenDefID)
	if err != nil || row.RetiredAt.IsZero() {
		t.Fatalf("the access token of an already-revoked sign-in is still live after the retry: %+v, %v", row, err)
	}
}
