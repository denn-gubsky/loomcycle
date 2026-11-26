package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P13 (D-P13-2, owner ruling Q2): a tenant operator mints a user's
// personal worker token on the per-user mint with purpose personal_worker.

const personalWorkerBody = `{"purpose":"personal_worker"}`

// The mint needs workspace.personal_workers on and an enabled user of the
// caller's own tenant; it makes a FROZEN [workers:attach] token marked
// worker_personal, bound to that user, and nothing about it comes from the
// request or the user's access mode (an isolated member may have one, Q4).
func TestMintUserToken_PersonalWorkerNeedsTheSwitchAndAnEnabledUserOfTheTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	s.cfg().Env.TokenAuthorityInherit = true // the default must not reach a worker token
	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedUser(t, st, "acme", "carol", "tenant", "disabled")
	seedUser(t, st, "other", "dave", "tenant", "active")

	if code, out, row := rawMint(t, s, st, personalWorkerBody); code != http.StatusForbidden || row.DefID != "" {
		t.Fatalf("switch off: %d %v, want 403 and no row", code, out)
	}
	s.cfg().Workspace.PersonalWorkers = true

	code, out, row := rawMint(t, s, st, personalWorkerBody)
	if code != http.StatusCreated {
		t.Fatalf("mint: %d %v", code, out)
	}
	if !slices.Equal(row.AllowedScopes, []string{auth.ScopeWorkersAttach}) || row.TenantID != "acme" || row.Subject != "alice" {
		t.Errorf("row = scopes %v tenant %q subject %q", row.AllowedScopes, row.TenantID, row.Subject)
	}
	if row.Authority != "" {
		t.Errorf("authority = %q, want frozen", row.Authority)
	}
	r, err := auth.ParseRestrictions(row.RestrictionsJSON)
	if err != nil || !r.WorkerPersonal || r.WorkerShared {
		t.Errorf("restrictions %q: %+v, %v", row.RestrictionsJSON, r, err)
	}
	if !strings.HasPrefix(row.Name, auth.UserTokenNamePrefix) || !strings.HasSuffix(row.Name, "-worker") {
		t.Errorf("name = %q", row.Name)
	}
	tok, _ := out["token"].(string)
	p, ok := s.resolvePrincipal(context.Background(), tok)
	if !ok {
		t.Fatal("the minted token does not resolve")
	}
	att, err := auth.WorkerAttachment(p)
	if err != nil || !att.Personal || att.UserID != "alice" || att.TenantID != "acme" {
		t.Errorf("attachment = %+v, %v; want personal acme/alice", att, err)
	}

	for name, sub := range map[string]string{"another tenant's user": "dave", "a disabled user": "carol", "no such user": "erin"} {
		rec := httptest.NewRecorder()
		s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/"+sub+"/tokens", personalWorkerBody, tenantOp, "subject", sub))
		if rec.Code == http.StatusCreated {
			t.Errorf("%s: minted", name)
		}
	}
	// Nothing else of a person's token rides along.
	for _, body := range []string{
		`{"purpose":"personal_worker","agents":["chat"]}`,
		`{"purpose":"personal_worker","retention":"none"}`,
		`{"purpose":"personal_worker","authority":"inherit"}`,
		`{"purpose":"personal_worker","max_active":1}`,
		`{"purpose":"personal_worker","surfaces":["speech"]}`,
		`{"purpose":"something_else"}`,
	} {
		if code, out, _ := rawMint(t, s, st, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", body, code, out)
		}
	}
}

// A member never mints a personal worker token, for itself or anyone: the
// route refuses a member token, and the handler itself asks for a tenant
// operator, so a route mistake cannot open it.
func TestMintUserToken_PersonalWorkerRefusedForAMemberCaller(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	s.cfg().Workspace.PersonalWorkers = true
	seedUser(t, st, "acme", "bob", "tenant", "active")
	seedToken(t, st, "member-bearer", "acme", "bob", eqMemberScopes, time.Time{})
	if reached, code := runMW(t, s, "POST", "/v1/_users/bob/tokens", "member-bearer"); reached || code != http.StatusForbidden {
		t.Errorf("route: reached=%v code=%d, want refused 403", reached, code)
	}
	member := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: eqMemberScopes}
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/bob/tokens", personalWorkerBody, member, "subject", "bob"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("handler, member caller: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// A personal worker token goes with its user: disabling or erasing the user
// retires it like the user's other tokens. And it is not one of the user's
// sign-in tokens, so a later mint's max_active never retires it.
func TestUserDisable_RetiresThePersonalWorkerToken(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	s.cfg().Workspace.PersonalWorkers = true
	seedUser(t, st, "acme", "alice", "tenant", "active")
	ctx := context.Background()
	_, _, worker := rawMint(t, s, st, personalWorkerBody)
	if worker.DefID == "" {
		t.Fatal("no worker token")
	}
	if code, _, _ := rawMint(t, s, st, `{"max_active":1}`); code != http.StatusCreated {
		t.Fatalf("a sign-in mint: %d", code)
	}
	if got, _ := st.OperatorTokenDefGet(ctx, worker.DefID); !got.RetiredAt.IsZero() {
		t.Error("a sign-in mint's max_active retired the worker token")
	}
	revocable, _, err := s.erasureSubjectTokens(ctx, "acme", "alice")
	if err != nil || !slices.ContainsFunc(revocable, func(r store.OperatorTokenDefRow) bool { return r.DefID == worker.DefID }) {
		t.Errorf("erasure's revocable tokens leave out the worker token: %v", err)
	}
	if _, err := s.retireDelegatedUserTokens(ctx, "acme", "alice"); err != nil {
		t.Fatal(err)
	}
	got, err := st.OperatorTokenDefGet(ctx, worker.DefID)
	if err != nil || got.RetiredAt.IsZero() {
		t.Errorf("after the user's disable the worker token is not retired: %+v, %v", got.RetiredAt, err)
	}
}
