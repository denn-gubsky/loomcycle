package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// identityListStore records the linked-identity reads, so a test can hold
// that each one is keyed by the run's own identity.
type identityListStore struct {
	store.Store
	reads []string // "tenant/subject"
}

func (r *identityListStore) UserIdentityListSubject(ctx context.Context, tenant, subject string) ([]store.UserIdentityRow, error) {
	r.reads = append(r.reads, tenant+"/"+subject)
	return r.Store.UserIdentityListSubject(ctx, tenant, subject)
}

// seedIdentities links two identities to acme's alice — a verified Google
// account and an unverified password-gate address — and one to globex's
// alice, which must never surface in an acme run.
func seedIdentities(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	login := time.Date(2026, 10, 23, 7, 41, 12, 0, time.UTC)
	for _, row := range []store.UserIdentityRow{
		{TenantID: "acme", Subject: "alice", Gate: "google", Issuer: "https://accounts.google.com", ExternalSubject: "1182-ext", Email: "alice@acme.com", EmailVerified: true, LastLoginAt: login},
		{TenantID: "acme", Subject: "alice", Gate: "password", ExternalSubject: "alice-login", Email: "unverified@elsewhere.example"},
		{TenantID: "globex", Subject: "alice", Gate: "google", Issuer: "https://accounts.google.com", ExternalSubject: "9999-ext", Email: "alice@globex.example", EmailVerified: true},
	} {
		if err := st.UserIdentityCreate(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
}

// withLogin replaces the run's principal with one a Web UI session
// authenticated through gate.
func withLogin(ctx context.Context, gate string, at time.Time) context.Context {
	p, _ := auth.PrincipalFromContext(ctx)
	p.Login = &auth.Login{Gate: gate, AMR: []string{"oidc"}, AuthTime: at}
	return auth.WithPrincipal(ctx, p)
}

func identityBlock(t *testing.T, c *Context, ctx context.Context) (map[string]any, string) {
	t.Helper()
	out, res := callContext(t, c, ctx, `{"op":"self"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	id, ok := out["identity"].(map[string]any)
	if !ok {
		t.Fatalf("no identity block: %v", out)
	}
	return id, res.Text
}

func TestContextSelf_IdentityReportsTheSessionsSignInAndLinkedIdentities(t *testing.T) {
	st := identityStore(t)
	seedIdentities(t, st)
	at := time.Date(2026, 10, 23, 7, 41, 12, 0, time.UTC)
	ctx := withLogin(identityRun(t, st, authz.ModeCompat, "alice"), "google", at)
	id, _ := identityBlock(t, &Context{Store: st}, ctx)
	if got, want := asJSON(t, id["auth"]), `{"amr":["oidc"],"auth_time":"2026-10-23T07:41:12Z","gate":"google"}`; got != want {
		t.Errorf("auth = %s\nwant   %s", got, want)
	}
	want := `[{"email":"alice@acme.com","email_verified":true,"gate":"google","last_login_at":"2026-10-23T07:41:12Z"},{"email_verified":false,"gate":"password"}]`
	if got := asJSON(t, id["identities"]); got != want {
		t.Errorf("identities = %s\nwant         %s", got, want)
	}
}

// An address the gate did not verify is a claim, not a contact: it is not
// shown, even to the person's own agent.
func TestContextSelf_IdentityShowsAnEmailOnlyWhenVerified(t *testing.T) {
	st := identityStore(t)
	seedIdentities(t, st)
	_, text := identityBlock(t, &Context{Store: st}, identityRun(t, st, authz.ModeCompat, "alice"))
	if strings.Contains(text, "unverified@elsewhere.example") {
		t.Errorf("an unverified e-mail is reported: %s", text)
	}
	if !strings.Contains(text, "alice@acme.com") {
		t.Errorf("the verified e-mail is missing, so the check asserts nothing: %s", text)
	}
}

// Nothing that identifies the sign-in to a third party leaves the block: no
// external subject, issuer, token, session or credential reference.
func TestContextSelf_IdentityCarriesNoCredentialOrExternalSubject(t *testing.T) {
	st := identityStore(t)
	seedIdentities(t, st)
	ctx := withLogin(identityRun(t, st, authz.ModeCompat, "alice"), "google", time.Now())
	id, text := identityBlock(t, &Context{Store: st}, ctx)
	for _, leaked := range []string{"1182-ext", "alice-login", "accounts.google.com"} {
		if strings.Contains(text, leaked) {
			t.Errorf("op=self carries %q: %s", leaked, text)
		}
	}
	allowed := map[string]bool{"gate": true, "amr": true, "auth_time": true, "email": true, "email_verified": true, "last_login_at": true}
	check := func(where string, m map[string]any) {
		for k := range m {
			if !allowed[k] {
				t.Errorf("%s carries an unexpected key %q", where, k)
			}
		}
	}
	a, _ := id["auth"].(map[string]any)
	check("auth", a)
	ids, _ := id["identities"].([]any)
	if len(ids) == 0 {
		t.Fatal("no identities; the key census asserts nothing")
	}
	for _, e := range ids {
		m, _ := e.(map[string]any)
		check("identities[]", m)
	}
}

// Every linked-identity read is the run's own (tenant, subject), whatever
// the call carries; another tenant's alice never surfaces.
func TestContextSelf_IdentityLinkedIdentitiesAreReadForTheRunsOwnSubject(t *testing.T) {
	inner := identityStore(t)
	seedIdentities(t, inner)
	rec := &identityListStore{Store: inner}
	ctx := identityRun(t, inner, authz.ModeCompat, "alice")
	_, text := identityBlock(t, &Context{Store: rec}, ctx)
	if strings.Contains(text, "globex") {
		t.Errorf("another tenant's identity surfaced: %s", text)
	}
	if len(rec.reads) != 1 || rec.reads[0] != "acme/alice" {
		t.Errorf("identity reads = %v, want exactly [acme/alice]", rec.reads)
	}
}

// An operator token went through no gate: no auth block. A token a gate
// issued names the gate.
func TestContextSelf_IdentityAuthFollowsTheCredential(t *testing.T) {
	st := identityStore(t)
	ctx := identityRun(t, st, authz.ModeCompat, "alice")
	id, _ := identityBlock(t, &Context{Store: st}, ctx)
	if a, ok := id["auth"]; ok {
		t.Errorf("an operator token reports a sign-in: %v", a)
	}
	if _, ok := id["identities"]; ok {
		t.Errorf("a subject with no linked identity reports some: %v", id["identities"])
	}
	p, _ := auth.PrincipalFromContext(ctx)
	p.IssuedByGate = "google"
	id, _ = identityBlock(t, &Context{Store: st}, auth.WithPrincipal(ctx, p))
	if got := asJSON(t, id["auth"]); got != `{"gate":"google"}` {
		t.Errorf("gate-issued token auth = %s", got)
	}
}

// The principal on ctx is reported only when it is the subject the block is
// about: a sign-in of one person is never attributed to another.
func TestContextSelf_IdentityAuthIsNotReportedForAnotherSubject(t *testing.T) {
	st := identityStore(t)
	ctx := identityRun(t, st, authz.ModeCompat, "alice")
	// The run's set says alice; the principal stamped on ctx is bob's session.
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "bob", Login: &auth.Login{Gate: "google"}})
	id, _ := identityBlock(t, &Context{Store: st}, ctx)
	if id["subject"] != "alice" {
		t.Fatalf("subject = %v", id["subject"])
	}
	if a, ok := id["auth"]; ok {
		t.Errorf("bob's sign-in is reported as alice's: %v", a)
	}
}
