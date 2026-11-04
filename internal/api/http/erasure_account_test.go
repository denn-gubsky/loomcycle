package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Erasure ends the account (RFC FY Part F, audit B8). The subject under test
// is the app fixture's identity-gate user, google:100 in acme, signed in to
// the "board" app.

func eraseAppUser(t *testing.T, f *appFixture, dryRun bool) erasure.Result {
	t.Helper()
	f.s.SetEraseAudit(&recordingSink{})
	res, err := f.s.ErasureExecute(context.Background(), erasure.ExecuteRequest{
		Tenant: "acme", Subject: "google:100", DryRun: dryRun, Confirm: "google:100"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestErasure_ARefreshTokenIsDeadAfterwards(t *testing.T) {
	f := newAppFixture(t, "tenant")
	at, rt := f.signIn(t)
	res := eraseAppUser(t, f, false)
	if rec, body := f.refresh(t, rt); rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refresh after erasure = %d %v, want 400 invalid_grant", rec.Code, body)
	}
	if c, _ := f.bearerMe(t, at); c == http.StatusOK {
		t.Fatal("the app's access token still authenticates after the erasure")
	}
	// The app sign-in's access token ends with the sign-in, before the
	// token step runs; the report still names it.
	if res.Deleted["operator_tokens"] != 1 || len(res.Access.Tokens) != 1 {
		t.Fatalf("operator_tokens = %d, access.tokens = %+v; want the app's access token", res.Deleted["operator_tokens"], res.Access.Tokens)
	}
}

func TestErasure_TheSubjectCannotSignInAgain(t *testing.T) {
	t.Run("oidc identity", func(t *testing.T) {
		f := newAppFixture(t, "tenant")
		eraseAppUser(t, f, false)
		g, _ := f.s.identityGate("google")
		if out, err := f.s.provisionOrRefuse(context.Background(), g, assertion("100", nil), ""); err == nil {
			t.Fatalf("the erased subject signed in again through its identity provider: %+v", out)
		}
	})
	t.Run("password", func(t *testing.T) {
		f := newGateFixture(t)
		f.srv.SetEraseAudit(&recordingSink{})
		inv := f.inviteCode(t, bAcmeOp, "local", `{}`)
		b := f.redeemed(t, "local", "alice@example.com", inv)
		_, subject := b.me()
		if _, err := f.srv.ErasureExecute(context.Background(), erasure.ExecuteRequest{
			Tenant: "acme", Subject: subject, Confirm: subject}); err != nil {
			t.Fatal(err)
		}
		if code, body := f.browser(t).login("local", "alice@example.com", goodPassword, ""); code == http.StatusNoContent {
			t.Fatalf("the erased subject signed in again with its password: %d %v", code, body)
		}
		// An operator's invite naming the tombstone cannot give it a login.
		if code, body := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/invites",
			jsonBody(t, map[string]string{"subject": subject})); code != http.StatusConflict || body["code"] != "user_erased" {
			t.Fatalf("an invite for the erased subject = %d %v, want 409 user_erased", code, body)
		}
		if ids, _ := f.st.UserIdentityListSubject(context.Background(), "acme", subject); len(ids) != 0 {
			t.Fatalf("the erased subject has a login again: %+v", ids)
		}
	})
}

func TestErasure_IdentitiesAreGoneAndReported(t *testing.T) {
	f := newAppFixture(t, "tenant")
	f.signIn(t)
	ctx := context.Background()
	name := "Alice Example"
	if err := f.st.UserUpdate(ctx, "acme", "google:100", store.UserPatch{DisplayName: &name}); err != nil {
		t.Fatal(err)
	}

	dry := eraseAppUser(t, f, true)
	for _, plane := range []string{"sign_in_identities", "sign_in_app_families", "sign_in_sessions", "account"} {
		if dry.Deleted[plane] != 1 {
			t.Errorf("dry run %s = %d, want 1 (deleted = %v)", plane, dry.Deleted[plane], dry.Deleted)
		}
	}
	if dry.Deleted["sign_in_app_refresh_tokens"] < 1 {
		t.Errorf("dry run counts no refresh token: %v", dry.Deleted)
	}
	rep, err := f.s.ErasureReport(ctx, "acme", "google:100")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tier2.Counts["sign_in_identities"] != 1 || rep.Tier2.Counts["sign_in_app_families"] != 1 {
		t.Errorf("report tier2 = %v, want the sign-in rows counted", rep.Tier2.Counts)
	}
	if ids, _ := f.st.UserIdentityListSubject(ctx, "acme", "google:100"); len(ids) != 1 {
		t.Fatalf("the dry run removed the identity: %+v", ids)
	}

	res := eraseAppUser(t, f, false)
	for _, plane := range []string{"sign_in_identities", "sign_in_app_families", "sign_in_sessions", "account"} {
		if res.Deleted[plane] != 1 {
			t.Errorf("erasure %s = %d, want 1 (deleted = %v)", plane, res.Deleted[plane], res.Deleted)
		}
	}
	if ids, err := f.st.UserIdentityListSubject(ctx, "acme", "google:100"); err != nil || len(ids) != 0 {
		t.Fatalf("identities after erasure = %+v, %v", ids, err)
	}
	u, err := f.st.UserGet(ctx, "acme", "google:100")
	if err != nil {
		t.Fatalf("the tombstone is gone: %v", err)
	}
	if u.DisplayName != "" || u.Status != "disabled" || u.ErasedAt.IsZero() {
		t.Fatalf("tombstone = %+v, want no display name, status disabled and erased_at set", u)
	}
	if res.Retained["account_tombstone"] == "" {
		t.Errorf("retained = %v, want the tombstone named", res.Retained)
	}
}

func TestErasure_TheTombstoneCannotBeReEnabledDeletedOrRecreated(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ts := appOpServer(t, f)
	eraseAppUser(t, f, false)
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/v1/_users/google:100", `{"status":"active"}`},
		{"PATCH", "/v1/_users/google:100", `{"display_name":"Back Again"}`},
		{"DELETE", "/v1/_users/google:100", ``},
		{"POST", "/v1/_users", `{"subject":"google:100","access_mode":"tenant"}`},
	} {
		if code, body := bearerCall(t, ts, appOpBearer, c.method, c.path, c.body); code != http.StatusConflict {
			t.Errorf("%s %s %s = %d %s, want 409", c.method, c.path, c.body, code, body)
		}
	}
	u, err := f.st.UserGet(context.Background(), "acme", "google:100")
	if err != nil || u.Status != "disabled" || u.DisplayName != "" {
		t.Fatalf("tombstone after the attempts = %+v, %v", u, err)
	}
}
