package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The app sign-in routes: an operator lists and ends a user's app sign-ins,
// a person its own, and neither reaches anyone else's.

func appSignInList(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out struct {
		AppSignIns []map[string]any `json:"app_sign_ins"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out.AppSignIns
}

func TestAppSignIns_AnOperatorListsAndEndsAUsersSignIns(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ts := appOpServer(t, f)
	at1, rt1 := f.signIn(t)
	_, rt2 := f.signIn(t)

	code, body := bearerCall(t, ts, appOpBearer, "GET", "/v1/_users/google:100/app-sign-ins", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	list := appSignInList(t, body)
	if len(list) != 2 {
		t.Fatalf("listed %d sign-ins, want 2: %s", len(list), body)
	}
	for _, s := range list {
		for _, k := range []string{"family_id", "client_id", "client_name", "gate", "created_at", "last_used_at", "expires_at"} {
			if _, ok := s[k]; !ok {
				t.Errorf("a sign-in lacks %q: %v", k, s)
			}
		}
		if len(s) != 7 {
			t.Errorf("a sign-in carries fields beyond the documented seven: %v", s)
		}
	}
	// No token material: neither the refresh nor the access token, nor
	// anything shaped like one.
	for _, secret := range []string{rt1, rt2, at1} {
		if strings.Contains(body, secret) {
			t.Fatal("the listing carries a token")
		}
	}

	fam := list[0]["family_id"].(string)
	if code, body := bearerCall(t, ts, appOpBearer, "DELETE", "/v1/_users/google:100/app-sign-ins/"+fam, ""); code != http.StatusNoContent {
		t.Fatalf("end = %d %s", code, body)
	}
	row, err := f.st.OAuthFamilyGet(context.Background(), fam)
	if err != nil || row.RevokedAt.IsZero() || row.RevokeReason != oauthRevokeByOperator {
		t.Fatalf("the ended family = %+v, %v", row, err)
	}
	ended, kept := rt1, rt2
	if list[0]["family_id"] != familyOfRefresh(t, f.st, rt1) {
		ended, kept = rt2, rt1
	}
	if rec, body := f.refresh(t, ended); rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refresh of the ended sign-in = %d %v", rec.Code, body)
	}
	if rec, _ := f.refresh(t, kept); rec.Code != http.StatusOK {
		t.Fatalf("the other sign-in stopped refreshing: %d", rec.Code)
	}
	if code, _ := bearerCall(t, ts, appOpBearer, "GET", "/v1/_users/google:100/app-sign-ins", ""); code != http.StatusOK {
		t.Fatal("list after end")
	}
	// Ending it again, or a family that is not this user's, is the same 404.
	for _, path := range []string{"/v1/_users/google:100/app-sign-ins/" + fam, "/v1/_users/google:100/app-sign-ins/no-such-family"} {
		if code, body := bearerCall(t, ts, appOpBearer, "DELETE", path, ""); code != http.StatusNotFound || !strings.Contains(body, "app_sign_in_not_found") {
			t.Errorf("DELETE %s = %d %s", path, code, body)
		}
	}
}

func familyOfRefresh(t *testing.T, st store.Store, rt string) string {
	t.Helper()
	row, err := st.OAuthRefreshGet(context.Background(), hashOAuthSecret(rt))
	if err != nil {
		t.Fatal(err)
	}
	return row.FamilyID
}

// Another tenant's operator, or a family that belongs to another user of the
// tenant, reaches nothing — and learns nothing from the answer.
func TestAppSignIns_AnOperatorReachesOnlyItsTenantsUserAndThatUsersSignIns(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ts := appOpServer(t, f)
	f.signIn(t)
	fam := liveFamilyOf(t, f.st, "google:100")
	seedToken(t, f.st, "other-op-bearer", "other", "other-op", []string{auth.ScopeTenant}, time.Time{})
	if code, body := bearerCall(t, ts, "other-op-bearer", "GET", "/v1/_users/google:100/app-sign-ins", ""); code != http.StatusNotFound {
		t.Fatalf("another tenant's operator listed = %d %s", code, body)
	}
	if code, _ := bearerCall(t, ts, "other-op-bearer", "DELETE", "/v1/_users/google:100/app-sign-ins/"+fam, ""); code != http.StatusNotFound {
		t.Fatalf("another tenant's operator ended = %d", code)
	}
	if err := f.st.UserCreate(context.Background(), store.UserRow{TenantID: "acme", Subject: "bystander", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if code, body := bearerCall(t, ts, appOpBearer, "DELETE", "/v1/_users/bystander/app-sign-ins/"+fam, ""); code != http.StatusNotFound || !strings.Contains(body, "app_sign_in_not_found") {
		t.Fatalf("ending google:100's sign-in as the bystander's = %d %s", code, body)
	}
	if row, _ := f.st.OAuthFamilyGet(context.Background(), fam); !row.RevokedAt.IsZero() {
		t.Fatal("a refused end revoked the family")
	}
}

func liveFamilyOf(t *testing.T, st store.Store, subject string) string {
	t.Helper()
	rows, err := st.OAuthFamilyList(context.Background(), "acme", subject, time.Now())
	if err != nil || len(rows) == 0 {
		t.Fatalf("no live family for %s: %v", subject, err)
	}
	return rows[0].FamilyID
}

// A member is not an operator: the /v1/_users routes refuse it, its own
// /v1/_me routes answer for its own sign-ins only.
func TestAppSignIns_APersonSeesAndEndsOnlyItsOwn(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ts := appOpServer(t, f)
	f.signIn(t)
	_, rt2 := f.signIn(t)
	mine := familyOfRefresh(t, f.st, rt2)
	// The person's own token: an app's token is refused here
	// (TestAppSignIns_AnAppTokenCannotReachThePersonsSignIns).
	at := mintPersonalToken(t, ts, "google:100")
	seedToken(t, f.st, "peer-bearer", "acme", "peer", []string{auth.ScopeRunsRead, auth.ScopeRunsCreate}, time.Time{})

	if code, _ := bearerCall(t, ts, at, "GET", "/v1/_users/google:100/app-sign-ins", ""); code != http.StatusForbidden {
		t.Fatalf("a member reached the operator route: %d", code)
	}
	code, body := bearerCall(t, ts, at, "GET", "/v1/_me/app-sign-ins", "")
	if code != http.StatusOK || len(appSignInList(t, body)) != 2 {
		t.Fatalf("own list = %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, "peer-bearer", "GET", "/v1/_me/app-sign-ins", ""); code != http.StatusOK || len(appSignInList(t, body)) != 0 {
		t.Fatalf("a peer's own list = %d %s", code, body)
	}
	if code, _ := bearerCall(t, ts, "peer-bearer", "DELETE", "/v1/_me/app-sign-ins/"+mine, ""); code != http.StatusNotFound {
		t.Fatalf("a peer ended another person's sign-in: %d", code)
	}
	if code, body := bearerCall(t, ts, at, "DELETE", "/v1/_me/app-sign-ins/"+mine, ""); code != http.StatusNoContent {
		t.Fatalf("own end = %d %s", code, body)
	}
	row, _ := f.st.OAuthFamilyGet(context.Background(), mine)
	if row.RevokedAt.IsZero() || row.RevokeReason != oauthRevokeByUser {
		t.Fatalf("own end left %+v", row)
	}
	if rec, _ := f.refresh(t, rt2); rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh after signing the app out = %d", rec.Code)
	}
}

// An operator denied a user by its access control can neither list nor end
// that user's sign-ins; a user it is not denied stays reachable.
func TestAppSignIns_AnOperatorIsHeldToItsACL(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeCompat)
	ctx := context.Background()
	const opBearer = "lct_appsignin_acl_operator"
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: "def_op", Name: "op", TenantID: "acme",
		Subject: "op", TokenHash: auth.HashToken(testTokenPepper, opBearer), AllowedScopes: []string{auth.ScopeTenant}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, sub := range []string{"victim", "other"} {
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := st.OAuthFamilyCreate(ctx, store.OAuthFamilyRow{FamilyID: "fam-" + sub, ClientID: "board", TenantID: "acme",
			Subject: sub, Gate: "idp", CreatedAt: now, LastUsedAt: now, IdleExpiresAt: now.Add(time.Hour),
			AbsoluteExpiresAt: now.Add(24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	putACL(t, st, "hands-off", `[{"effect":"deny","actions":["read","manage"],"resources":["principal:user:victim"]}]`)
	bind(t, st, "hands-off", "user", "op")
	srv := httptestServerAt(url)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/_users/victim/app-sign-ins"},
		{"DELETE", "/v1/_users/victim/app-sign-ins/fam-victim"},
	} {
		if code, body := bearerCall(t, srv, opBearer, c.method, c.path, ""); code != http.StatusForbidden || !strings.Contains(body, "acl_denied") {
			t.Errorf("%s %s = %d %s", c.method, c.path, code, body)
		}
	}
	if row, _ := st.OAuthFamilyGet(ctx, "fam-victim"); !row.RevokedAt.IsZero() {
		t.Fatal("the victim's sign-in was ended")
	}
	if code, body := bearerCall(t, srv, opBearer, "GET", "/v1/_users/other/app-sign-ins", ""); code != http.StatusOK || len(appSignInList(t, body)) != 1 {
		t.Errorf("an undenied user = %d %s", code, body)
	}
}

// The connector methods the gRPC twins call ask the operator route's own
// question, built from the subject: the same surface authz.HTTPSurface
// answers for the route.
func TestAppSignIns_TheTwinsAskTheRoutesQuestion(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		path := "/v1/_users/alice/app-sign-ins"
		if method == http.MethodDelete {
			path += "/fam"
		}
		want, governed := authz.HTTPSurface(method, path)
		if got := appSignInSurface(method, "alice"); !governed || got != want {
			t.Errorf("%s: twin asks %+v, the route %+v (governed %v)", method, got, want, governed)
		}
	}
}
