package http

import (
	"context"
	"errors"
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

// failingAppSignInStore fails every app sign-in revoke.
type failingAppSignInStore struct{ store.Store }

func (failingAppSignInStore) OAuthFamilyRevokeSubject(context.Context, string, string, string, time.Time) ([]string, error) {
	return nil, errors.New("injected revoke fault")
}

func (failingAppSignInStore) OAuthTokenRetireEndedSignIns(context.Context, string, string, time.Time) (int, error) {
	return 0, errors.New("injected retire fault")
}

// TestRemoval_AFailedAppSignInEndStillRetiresTheOtherCredentials: a fault
// ending the app sign-ins does not stop the disable from retiring the
// user's tokens and ending its sessions; the disable reports the fault.
func TestRemoval_AFailedAppSignInEndStillRetiresTheOtherCredentials(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ctx := context.Background()
	at, _ := f.signIn(t)
	p, ok := f.s.resolvePrincipal(ctx, at)
	if !ok {
		t.Fatal("precondition: the app's access token does not authenticate")
	}
	f.s.store = failingAppSignInStore{f.st}
	if err := f.s.startRemoval(ctx, "acme", "google:100", "", store.RemovalReasonDormant, "test"); err == nil {
		t.Fatal("the removal hid the app sign-in fault")
	}
	row, err := f.st.OperatorTokenDefGet(ctx, p.TokenDefID)
	if err != nil || row.RetiredAt.IsZero() {
		t.Fatalf("the user's token outlived a disable whose app sign-in step failed: %+v, %v", row, err)
	}
	if code, _ := meOf(t, f.s, f.cookie); code == http.StatusOK {
		t.Fatal("the user's Web UI session outlived the disable")
	}
}

// seedLiveFamily writes a live app sign-in for (tenant, subject), standing
// for one a code exchange created while the user was being disabled.
func seedLiveFamily(t *testing.T, st store.Store, id, tenant, subject string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.OAuthFamilyCreate(context.Background(), store.OAuthFamilyRow{FamilyID: id, ClientID: "board", TenantID: tenant,
		Subject: subject, Gate: "google", CreatedAt: now, LastUsedAt: now,
		IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func familyRevoked(t *testing.T, st store.Store, id string) bool {
	t.Helper()
	f, err := st.OAuthFamilyGet(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return !f.RevokedAt.IsZero()
}

// TestReEnable_EndsAnyAppSignInLeftLive: no app sign-in can be made while a
// user is disabled, so one live at the re-enable came from a code exchange
// that raced the disable. Every re-enable path ends it.
func TestReEnable_EndsAnyAppSignInLeftLive(t *testing.T) {
	t.Run("operator patch", func(t *testing.T) {
		f := newAppFixture(t, "tenant")
		ts := appOpServer(t, f)
		if code, body := bearerCall(t, ts, appOpBearer, "PATCH", "/v1/_users/google:100", `{"status":"disabled"}`); code != http.StatusOK {
			t.Fatalf("disable = %d %s", code, body)
		}
		seedLiveFamily(t, f.st, "fam-raced", "acme", "google:100")
		if code, body := bearerCall(t, ts, appOpBearer, "PATCH", "/v1/_users/google:100", `{"status":"active"}`); code != http.StatusOK {
			t.Fatalf("re-enable = %d %s", code, body)
		}
		if !familyRevoked(t, f.st, "fam-raced") {
			t.Fatal("an app sign-in made during the disable survived the re-enable")
		}
	})
	t.Run("active user keeps them", func(t *testing.T) {
		f := newAppFixture(t, "tenant")
		ts := appOpServer(t, f)
		_, rt := f.signIn(t)
		if code, body := bearerCall(t, ts, appOpBearer, "PATCH", "/v1/_users/google:100", `{"status":"active"}`); code != http.StatusOK {
			t.Fatalf("no-op re-enable = %d %s", code, body)
		}
		if rec, body := f.refresh(t, rt); rec.Code != http.StatusOK {
			t.Fatalf("a no-op status patch ended an active user's app sign-in: %d %v", rec.Code, body)
		}
	})
	t.Run("removal reactivate", func(t *testing.T) {
		f := newAppFixture(t, "tenant")
		ts := appOpServer(t, f)
		if err := f.s.startRemoval(context.Background(), "acme", "google:100", "", store.RemovalReasonDormant, "test"); err != nil {
			t.Fatal(err)
		}
		seedLiveFamily(t, f.st, "fam-raced", "acme", "google:100")
		if code, body := bearerCall(t, ts, appOpBearer, "PATCH", "/v1/_users/google:100", `{"status":"active"}`); code != http.StatusOK {
			t.Fatalf("re-enable = %d %s", code, body)
		}
		if !familyRevoked(t, f.st, "fam-raced") {
			t.Fatal("an app sign-in made during the removal survived the re-enable")
		}
	})
	t.Run("scim reactivate", func(t *testing.T) {
		f := newSCIMFixture(t, scimOpts{authoritative: true})
		id, subject := f.scimUser(t, bSCIMAcme, "alice@acme.test")
		off := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": false})
		on := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": true})
		if code, out := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, off); code != http.StatusOK {
			t.Fatalf("SCIM disable = %d %v", code, out)
		}
		seedLiveFamily(t, f.st, "fam-raced", "acme", subject)
		if code, out := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, on); code != http.StatusOK {
			t.Fatalf("SCIM re-enable = %d %v", code, out)
		}
		if !familyRevoked(t, f.st, "fam-raced") {
			t.Fatal("an app sign-in made during the SCIM removal survived the re-enable")
		}
	})
}
