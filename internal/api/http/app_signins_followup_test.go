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
