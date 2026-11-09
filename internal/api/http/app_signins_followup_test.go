package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// retireFailsOnce is a store whose first OAuthTokenRetire faults: the step
// after the family revoke.
type retireFailsOnce struct {
	store.Store
	failed atomic.Bool
}

func (s *retireFailsOnce) OAuthTokenRetire(ctx context.Context, clientID, familyID string, at time.Time) (int, error) {
	if s.failed.CompareAndSwap(false, true) {
		return 0, errors.New("injected: token retire failed")
	}
	return s.Store.OAuthTokenRetire(ctx, clientID, familyID, at)
}

// Ending an app sign-in that faulted after the family was revoked finishes
// on a retry: the family is already revoked, and its access tokens still
// go.
func TestAppSignIns_ARetriedEndFinishesAfterAPartialFailure(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ts := appOpServer(t, f)
	at, _ := f.signIn(t)
	fam := liveFamilyOf(t, f.st, "google:100")
	f.s.store = &retireFailsOnce{Store: f.st}
	path := "/v1/_users/google:100/app-sign-ins/" + fam
	if code, body := bearerCall(t, ts, appOpBearer, "DELETE", path, ""); code != http.StatusInternalServerError {
		t.Fatalf("precondition: the injected fault did not fail the end: %d %s", code, body)
	}
	code, body := bearerCall(t, ts, appOpBearer, "DELETE", path, "")
	if code != http.StatusNoContent && code != http.StatusNotFound {
		t.Fatalf("retry = %d %s", code, body)
	}
	if code, _ := f.bearerMe(t, at); code != http.StatusUnauthorized {
		t.Fatalf("the ended sign-in's access token still answers /v1/_me: %d", code)
	}
}

// Removing a passkey — a factor that may be lost — ends the app sign-ins
// and delegated tokens made while it was on the account; the browser that
// removed it, signed in with the password, stays signed in.
func TestPasskey_RemovingAPasskeyEndsAppSignInsAndTokens(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "pia@example.com")
	a := registerPasskey(t, b)
	seedAppSignIn(t, f.st, "fam-passkey", sub)
	if code, out := f.call(t, bAcmeOp, "POST", "/v1/_users/"+sub+"/tokens", `{}`); code != http.StatusCreated {
		t.Fatalf("mint a user token: %d %v", code, out)
	}
	path := "/v1/_auth/passkeys/" + b64u.EncodeToString(a.credID)
	_, out := b.do("DELETE", path, passwordStepUp)
	body := map[string]any{"current_password": goodPassword, "webauthn": a.get(out["webauthn"].(map[string]any))}
	if code, out := b.do("DELETE", path, jsonBody(t, body)); code != http.StatusNoContent {
		t.Fatalf("remove: %d %v", code, out)
	}
	assertFamilyRevoked(t, f.st, "fam-passkey", "a passkey removal")
	assertNoActiveDelegatedToken(t, f.st, sub, "a passkey removal")
	if code, _ := b.me(); code != http.StatusOK {
		t.Fatalf("the password browser that removed the passkey lost its session: %d", code)
	}
}

// teardownBeforeCreate runs a full teardown of (acme, subject) the moment a
// sign-in is about to be written — after the exchange or redemption checked
// the approving session, before its family or token exists: the
// interleaving where a credential change lands mid-exchange.
type teardownBeforeCreate struct {
	store.Store
	s       *Server
	subject string
	once    atomic.Bool
}

func (w *teardownBeforeCreate) teardown(ctx context.Context) {
	if w.once.CompareAndSwap(false, true) {
		if _, err := w.s.endSubjectCredentials(ctx, "acme", w.subject, credAll); err != nil {
			panic(err)
		}
	}
}

func (w *teardownBeforeCreate) OAuthFamilyCreate(ctx context.Context, row store.OAuthFamilyRow) error {
	w.teardown(ctx)
	return w.Store.OAuthFamilyCreate(ctx, row)
}

func (w *teardownBeforeCreate) OperatorTokenDefCreate(ctx context.Context, row store.OperatorTokenDefRow) (store.OperatorTokenDefRow, error) {
	w.teardown(ctx)
	return w.Store.OperatorTokenDefCreate(ctx, row)
}

// A code exchange that a credential change overtakes leaves no live app
// sign-in: the family it then creates is ended at once.
func TestAppSignIns_AnExchangeOvertakenByATeardownLeavesNoSignIn(t *testing.T) {
	f := newAppFixture(t, "tenant")
	code := f.code(t)
	f.s.store = &teardownBeforeCreate{Store: f.st, s: f.s, subject: "google:100"}
	rec, body := f.exchange(t, code)
	if live, err := f.st.OAuthFamilyList(context.Background(), "acme", "google:100", time.Now()); err != nil || len(live) != 0 {
		t.Fatalf("a sign-in made across the teardown is live: %d families, err %v (exchange answered %d)", len(live), err, rec.Code)
	}
	if at, _ := body["access_token"].(string); at != "" {
		if code, _ := f.bearerMe(t, at); code != http.StatusUnauthorized {
			t.Fatalf("the exchange's access token answers /v1/_me: %d", code)
		}
	}
}

// A device login a credential change overtakes yields no working token.
func TestDeviceLogin_ARedemptionOvertakenByATeardownYieldsNoToken(t *testing.T) {
	s, st, _ := deviceServer(t)
	ds := startDevice(t, s)
	alice, _ := userSession(t, s, st, "acme", "alice", "isolated", "")
	if rec := decide(s, alice, "/v1/_auth/device/approve", ds.UserCode, yes); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if err := forcePollable(st, ds.DeviceCode); err != nil {
		t.Fatal(err)
	}
	s.store = &teardownBeforeCreate{Store: st, s: s, subject: "alice"}
	a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", "")
	rows, err := st.OperatorTokenDefListBySubject(context.Background(), "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if tokenActive(r) {
			t.Fatalf("a device token made across the teardown is active: %s (poll answered %d)", r.DefID, a.Status)
		}
	}
}

// mintPersonalToken mints a delegated token for the user as its operator
// would, and returns the bearer.
func mintPersonalToken(t *testing.T, ts *httptest.Server, subject string) string {
	t.Helper()
	code, body := bearerCall(t, ts, appOpBearer, "POST", "/v1/_users/"+subject+"/tokens", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("mint: %d", code)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Token == "" {
		t.Fatalf("mint answer unreadable: %v", err)
	}
	return out.Token
}

// An app's own token (issued through an app sign-in, capped to what the app
// was granted) cannot list or end the person's sign-ins: one app must not
// sign the person out of the others. The person's own token can.
func TestAppSignIns_AnAppTokenCannotReachThePersonsSignIns(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ts := appOpServer(t, f)
	at, _ := f.signIn(t)
	fam := liveFamilyOf(t, f.st, "google:100")
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/_me/app-sign-ins"},
		{"DELETE", "/v1/_me/app-sign-ins/" + fam},
	} {
		if code, _ := bearerCall(t, ts, at, c.method, c.path, ""); code != http.StatusForbidden {
			t.Errorf("an app token: %s %s = %d, want 403", c.method, c.path, code)
		}
	}
	if row, _ := f.st.OAuthFamilyGet(context.Background(), fam); !row.RevokedAt.IsZero() {
		t.Fatal("an app token ended a sign-in")
	}
	own := mintPersonalToken(t, ts, "google:100")
	if code, body := bearerCall(t, ts, own, "GET", "/v1/_me/app-sign-ins", ""); code != http.StatusOK || len(appSignInList(t, body)) != 1 {
		t.Fatalf("the person's own token = %d %s", code, body)
	}
}
