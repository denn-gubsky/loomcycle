package http

import (
	"context"
	"errors"
	"net/http"
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
