package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// captureSink keeps every audit record in memory.
type captureSink struct {
	mu  sync.Mutex
	evs []audit.Event
}

func (c *captureSink) Record(ev audit.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
	return nil
}

func (c *captureSink) actions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.evs {
		out = append(out, e.Action)
	}
	return out
}

const testSecretKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func testPasswordGates(t *testing.T) *PasswordGates {
	t.Helper()
	pepper, err := credential.DerivePasswordPepper(testSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	h, err := password.NewHasher(pepper, nil)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := password.NewPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.NewSealer(testSecretKey, "")
	if err != nil {
		t.Fatal(err)
	}
	gate := func(name string, trusted bool) PasswordGateSettings {
		return PasswordGateSettings{Name: name, Trusted: trusted, InviteDefaultTTL: 168 * time.Hour, InviteMaxTTL: 720 * time.Hour,
			ResetTTL: time.Hour, AllowedReturnTo: []string{"https://site.example/welcome"},
			Throttle: password.Throttle{MaxFailures: 10, Lockout: 15 * time.Minute}}
	}
	pg, err := NewPasswordGates([]PasswordGateSettings{gate("local", false), gate("staff", true)}, h, pol, sealer, "https://lc.example/")
	if err != nil {
		t.Fatal(err)
	}
	return pg
}

// gateFixture is a server with two password gates (local untrusted, staff
// trusted) and four callers: the legacy admin, a tenant operator of acme, one
// of other, and a website's auth:manage credential.
type gateFixture struct {
	srv   *Server
	st    store.Store
	ts    *httptest.Server
	audit *captureSink
}

const (
	bAdmin   = "legacy-admin"
	bAcmeOp  = "acme-op-bearer"
	bOtherOp = "other-op-bearer"
	bSite    = "site-bearer"
)

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	s, st := tokenAuthServer(t, bAdmin)
	seedToken(t, st, bAcmeOp, "acme", "acme-op", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, st, bOtherOp, "other", "other-op", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, st, bSite, "site", "site", []string{auth.ScopeAuthManage}, time.Time{})
	sink := &captureSink{}
	s.SetTokenAudit(sink)
	s.SetPasswordGates(testPasswordGates(t))
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	return &gateFixture{srv: s, st: st, ts: ts, audit: sink}
}

func (f *gateFixture) call(t *testing.T, bearer, method, path, body string) (int, map[string]any) {
	t.Helper()
	code, raw := bearerCall(t, f.ts, bearer, method, path, body)
	out := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &out)
	return code, out
}

func (f *gateFixture) addUser(t *testing.T, tenant, subject, mode string) {
	t.Helper()
	if err := f.st.UserCreate(context.Background(), store.UserRow{TenantID: tenant, Subject: subject, AccessMode: mode, Status: "active"}); err != nil {
		t.Fatal(err)
	}
}

func (f *gateFixture) addLogin(t *testing.T, tenant, subject, gate, login string) {
	t.Helper()
	if err := f.st.UserIdentityCreate(context.Background(), store.UserIdentityRow{TenantID: tenant, Subject: subject, Gate: gate, ExternalSubject: login, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func codeOf(t *testing.T, url, prefix string) string {
	t.Helper()
	i := strings.Index(url, "#")
	if i < 0 || !strings.HasPrefix(url, "https://lc.example/ui/") {
		t.Fatalf("link %q is not a loomcycle UI link with the code in the fragment", url)
	}
	code := url[i+1:]
	if !password.WellFormedCode(prefix, code) {
		t.Fatalf("link code %q is malformed", code)
	}
	return code
}

// Invite codes are shown once and stored only as their SHA-256: the row, the
// listing and the audit trail never carry the code (RFC FA security row "a DB
// read yields no usable link").
func TestAuthgateInvite_CodeIsShownOnceAndStoredOnlyHashed(t *testing.T) {
	f := newGateFixture(t)
	code, body := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/invites", `{"email":"alice@example.com"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	c := codeOf(t, body["url"].(string), password.InvitePrefix)
	row, err := f.st.AuthInviteGetByCode(context.Background(), password.HashCode(c))
	if err != nil {
		t.Fatalf("no row under the code's hash: %v", err)
	}
	if row.TenantID != "acme" || row.AccessMode != "isolated" || row.Bootstrap {
		t.Fatalf("row = %+v", row)
	}
	raw, _ := json.Marshal(row)
	secret := strings.TrimPrefix(c, password.InvitePrefix)
	if strings.Contains(string(raw), secret) {
		t.Fatal("the stored row contains the code")
	}
	_, list := bearerCall(t, f.ts, bAcmeOp, "GET", "/v1/_authgate/local/invites", "")
	if strings.Contains(list, secret) || !strings.Contains(list, row.InviteID) {
		t.Fatalf("listing leaks the code or misses the invite: %s", list)
	}
	ev, _ := json.Marshal(f.audit.evs)
	if strings.Contains(string(ev), secret) || !strings.Contains(string(ev), "auth_invite_created") {
		t.Fatalf("audit: %s", ev)
	}
}

func TestAuthgateInvite_TenantOperatorIsConfinedToItsOwnTenant(t *testing.T) {
	f := newGateFixture(t)
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/invites", `{"tenant":"other"}`); code != http.StatusForbidden {
		t.Fatalf("cross-tenant create: %d %v", code, b)
	}
	_, b := f.call(t, bOtherOp, "POST", "/v1/_authgate/local/invites", `{}`)
	id := b["invite_id"].(string)
	if code, _ := f.call(t, bAcmeOp, "DELETE", "/v1/_authgate/local/invites/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke: %d, want an opaque 404", code)
	}
	_, list := bearerCall(t, f.ts, bAcmeOp, "GET", "/v1/_authgate/local/invites", "")
	if strings.Contains(list, id) {
		t.Fatal("acme's operator lists other's invite")
	}
	if code, _ := f.call(t, bOtherOp, "DELETE", "/v1/_authgate/local/invites/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("own revoke: %d", code)
	}
	if code, _ := f.call(t, bOtherOp, "DELETE", "/v1/_authgate/local/invites/"+id, ""); code != http.StatusConflict {
		t.Fatalf("second revoke: %d, want 409", code)
	}
}

// An invite cannot grant beyond the floor on an untrusted gate, and an
// unknown or non-password gate is an opaque 404.
func TestAuthgateInvite_UntrustedGateHoldsTheInviterToTheFloor(t *testing.T) {
	f := newGateFixture(t)
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/invites", `{"access_mode":"tenant"}`); code != http.StatusForbidden || b["code"] != "beyond_floor" {
		t.Fatalf("tenant mode on untrusted: %d %v", code, b)
	}
	if _, err := f.st.GroupPut(context.Background(), store.GroupRow{TenantID: "acme", Name: "staff"}); err != nil {
		t.Fatal(err)
	}
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/invites", `{"groups":["staff"]}`); code != http.StatusForbidden {
		t.Fatalf("groups on untrusted: %d %v", code, b)
	}
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_authgate/staff/invites", `{"access_mode":"tenant","groups":["staff"]}`); code != http.StatusCreated {
		t.Fatalf("trusted gate: %d %v", code, b)
	}
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_authgate/staff/invites", `{"groups":["nope"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown group: %d %v", code, b)
	}
	if code, _ := f.call(t, bAcmeOp, "POST", "/v1/_authgate/token/invites", `{}`); code != http.StatusNotFound {
		t.Fatalf("non-password gate: %d", code)
	}
}

// The website bootstraps an empty tenant with one owner invite; once the
// tenant has a user it is held to the floor even on a trusted gate (D16).
func TestAuthgateInvite_BootstrapOwnerInviteOnlyWhileTheTenantIsEmpty(t *testing.T) {
	f := newGateFixture(t)
	if code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"access_mode":"tenant"}`); code != http.StatusBadRequest || b["code"] != "tenant_required" {
		t.Fatalf("no tenant: %d %v", code, b)
	}
	code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"newco","access_mode":"tenant","login":"Owner@NewCo.test"}`)
	if code != http.StatusCreated {
		t.Fatalf("bootstrap: %d %v", code, b)
	}
	row, _ := f.st.AuthInviteGet(context.Background(), b["invite_id"].(string))
	if !row.Bootstrap || row.Login != "owner@newco.test" {
		t.Fatalf("bootstrap row = %+v", row)
	}
	_, status := f.call(t, bSite, "GET", "/v1/_authgate/local/tenant-status?tenant=newco", "")
	if status["has_users"] != false || status["bootstrap_invite_live"] != true {
		t.Fatalf("tenant-status = %v", status)
	}
	if code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"newco","access_mode":"tenant"}`); code != http.StatusConflict {
		t.Fatalf("second live bootstrap: %d %v", code, b)
	}
	f.addUser(t, "newco", "owner", "tenant")
	for _, gate := range []string{"local", "staff"} {
		if code, b := f.call(t, bSite, "POST", "/v1/_authgate/"+gate+"/invites", `{"tenant":"newco","access_mode":"tenant"}`); code != http.StatusConflict && code != http.StatusForbidden {
			t.Fatalf("%s after a user exists: %d %v", gate, code, b)
		}
	}
	if _, err := f.st.AuthInviteRevoke(context.Background(), row.InviteID, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, gate := range []string{"local", "staff"} {
		if code, b := f.call(t, bSite, "POST", "/v1/_authgate/"+gate+"/invites", `{"tenant":"newco","access_mode":"tenant"}`); code != http.StatusForbidden || b["code"] != "tenant_not_empty" {
			t.Fatalf("%s: bootstrap on a non-empty tenant: %d %v", gate, code, b)
		}
	}
	if code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"newco"}`); code != http.StatusCreated {
		t.Fatalf("floor invite: %d %v", code, b)
	}
}

// An invite naming an existing user attaches a password to it (the
// coordinator's §6.1 addition): the user must exist, must not hold a login
// or password at the gate, and the invite can never raise what it has.
func TestAuthgateInvite_AttachToAnExistingUserIsNeverAPromotion(t *testing.T) {
	f := newGateFixture(t)
	if code, _ := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"acme","subject":"ghost"}`); code != http.StatusNotFound {
		t.Fatalf("missing user: %d", code)
	}
	f.addUser(t, "acme", "legacy-mail@example.com", "isolated")
	if code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"acme","subject":"legacy-mail@example.com","access_mode":"tenant"}`); code != http.StatusForbidden || b["code"] != "promotion_refused" {
		t.Fatalf("raise: %d %v", code, b)
	}
	if _, err := f.st.GroupPut(context.Background(), store.GroupRow{TenantID: "acme", Name: "staff"}); err != nil {
		t.Fatal(err)
	}
	if code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"acme","subject":"legacy-mail@example.com","groups":["staff"]}`); code != http.StatusForbidden {
		t.Fatalf("group add: %d %v", code, b)
	}
	code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"acme","subject":"legacy-mail@example.com","email":"legacy-mail@example.com"}`)
	if code != http.StatusCreated {
		t.Fatalf("attach: %d %v", code, b)
	}
	row, _ := f.st.AuthInviteGet(context.Background(), b["invite_id"].(string))
	if row.Subject != "legacy-mail@example.com" || row.AccessMode != "" || len(row.Groups) != 0 {
		t.Fatalf("attach row = %+v", row)
	}
	f.addLogin(t, "acme", "legacy-mail@example.com", "local", "legacy-mail@example.com")
	if code, b := f.call(t, bSite, "POST", "/v1/_authgate/local/invites", `{"tenant":"acme","subject":"legacy-mail@example.com"}`); code != http.StatusConflict || b["code"] != "credential_exists" {
		t.Fatalf("already has a login: %d %v", code, b)
	}
}

// D17: no management route accepts a password — by schema (no request type
// has a field that could carry one) and on the wire (an extra `password`
// key is refused, never silently dropped).
func TestAuthgateManage_NoRouteTakesAPassword(t *testing.T) {
	for _, typ := range []any{createInviteRequest{}, createResetRequest{}, renameLoginRequest{}, createUserRequest{}, updateUserRequest{}} {
		rt := reflect.TypeOf(typ)
		for i := range rt.NumField() {
			tag := strings.ToLower(rt.Field(i).Tag.Get("json") + rt.Field(i).Name)
			for _, bad := range []string{"password", "passwd", "secret", "totp", "phc", "credential"} {
				if strings.Contains(tag, bad) {
					t.Errorf("%s.%s can carry a %s", rt.Name(), rt.Field(i).Name, bad)
				}
			}
		}
	}
	f := newGateFixture(t)
	f.addUser(t, "acme", "bob", "isolated")
	f.addLogin(t, "acme", "bob", "local", "bob")
	for _, r := range []struct{ method, path, body string }{
		{"POST", "/v1/_authgate/local/invites", `{"password":"hunter2hunter2hunter2"}`},
		{"POST", "/v1/_authgate/local/resets", `{"subject":"bob","password":"hunter2hunter2hunter2"}`},
		{"PATCH", "/v1/_authgate/local/logins/bob", `{"login":"bobby","password":"hunter2hunter2hunter2"}`},
	} {
		if code, b := f.call(t, bAcmeOp, r.method, r.path, r.body); code != http.StatusBadRequest {
			t.Errorf("%s %s with a password field: %d %v, want 400", r.method, r.path, code, b)
		}
	}
}

func TestAuthgateReset_NeedsALoginAtTheGateAndSupersedesTheLastLink(t *testing.T) {
	f := newGateFixture(t)
	f.addUser(t, "acme", "carol", "isolated")
	if code, _ := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/resets", `{"subject":"carol"}`); code != http.StatusNotFound {
		t.Fatalf("no login: %d", code)
	}
	f.addLogin(t, "acme", "carol", "local", "carol")
	_, b1 := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/resets", `{"subject":"carol"}`)
	c1 := codeOf(t, b1["url"].(string), password.ResetPrefix)
	code, b2 := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/resets", `{"subject":"carol"}`)
	if code != http.StatusCreated {
		t.Fatalf("reset: %d %v", code, b2)
	}
	r1, _ := f.st.AuthResetGetByCode(context.Background(), password.HashCode(c1))
	if r1.Live(time.Now()) {
		t.Fatal("the first reset link still works after a second was issued")
	}
	if exp, _ := time.Parse(time.RFC3339, b2["expires_at"].(string)); time.Until(exp) > time.Hour+time.Minute {
		t.Fatalf("reset TTL %s, want 1h", time.Until(exp))
	}
	if code, _ := f.call(t, bOtherOp, "POST", "/v1/_authgate/local/resets", `{"subject":"carol"}`); code != http.StatusNotFound {
		t.Fatalf("another tenant's operator: %d", code)
	}
}

// Logins collide case-insensitively (RFC FA security row).
func TestAuthgateLogins_CollideCaseInsensitively(t *testing.T) {
	f := newGateFixture(t)
	f.addUser(t, "acme", "a", "isolated")
	f.addUser(t, "other", "b", "isolated")
	f.addLogin(t, "acme", "a", "local", "alice@example.com")
	f.addLogin(t, "other", "b", "local", "bob@example.com")
	if code, b := f.call(t, bOtherOp, "PATCH", "/v1/_authgate/local/logins/b", `{"login":"ALICE@Example.COM"}`); code != http.StatusConflict {
		t.Fatalf("rename onto another tenant's login (case differs): %d %v", code, b)
	}
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_authgate/local/invites", `{"login":"Alice@EXAMPLE.com"}`); code != http.StatusConflict {
		t.Fatalf("invite with a taken login: %d %v", code, b)
	}
	code, b := f.call(t, bOtherOp, "PATCH", "/v1/_authgate/local/logins/b", `{"login":"Robert@Example.com"}`)
	if code != http.StatusOK || b["login"] != "robert@example.com" {
		t.Fatalf("rename: %d %v", code, b)
	}
	_, ids := f.call(t, bOtherOp, "GET", "/v1/_authgate/local/identities?subject=b", "")
	if !strings.Contains(string(mustJSON(t, ids)), "robert@example.com") {
		t.Fatalf("identities = %v", ids)
	}
}

// The coordinator's D16 rule for the user roster: an auth:manage credential
// creates an access_mode tenant user only in an empty tenant, defaults to the
// floor otherwise, and can never raise a user's access_mode.
func TestUsersCRUD_AuthManagerIsHeldToTheFloorOutsideABootstrap(t *testing.T) {
	f := newGateFixture(t)
	if code, b := f.call(t, bSite, "POST", "/v1/_users", `{"subject":"owner"}`); code != http.StatusBadRequest || b["code"] != "tenant_required" {
		t.Fatalf("no tenant: %d %v", code, b)
	}
	// Tokens in a tenant with no users row (a legacy cloud tenant) do not
	// make it non-empty.
	seedToken(t, f.st, "legacy-cloud-token", "newco", "someone@newco.test", []string{auth.ScopeTenant}, time.Time{})
	if code, b := f.call(t, bSite, "POST", "/v1/_users?tenant=newco", `{"subject":"owner","access_mode":"tenant"}`); code != http.StatusCreated || b["tenant_id"] != "newco" {
		t.Fatalf("bootstrap owner: %d %v", code, b)
	}
	if code, b := f.call(t, bSite, "POST", "/v1/_users?tenant=newco", `{"subject":"second","access_mode":"tenant"}`); code != http.StatusForbidden || b["code"] != "tenant_not_empty" {
		t.Fatalf("second tenant user: %d %v", code, b)
	}
	if code, b := f.call(t, bSite, "POST", "/v1/_users?tenant=newco", `{"subject":"member"}`); code != http.StatusCreated || b["access_mode"] != "isolated" {
		t.Fatalf("default for the website is the floor: %d %v", code, b)
	}
	if code, b := f.call(t, bSite, "PATCH", "/v1/_users/member?tenant=newco", `{"access_mode":"tenant"}`); code != http.StatusForbidden || b["code"] != "promotion_refused" {
		t.Fatalf("raise: %d %v", code, b)
	}
	if code, b := f.call(t, bSite, "PATCH", "/v1/_users/owner?tenant=newco", `{"access_mode":"isolated","display_name":"O"}`); code != http.StatusOK {
		t.Fatalf("lowering is not a promotion: %d %v", code, b)
	}
	_, list := bearerCall(t, f.ts, bSite, "GET", "/v1/_users?tenant=newco", "")
	if !strings.Contains(list, `"owner"`) || !strings.Contains(list, `"member"`) || strings.Contains(list, "acme") {
		t.Fatalf("roster = %s", list)
	}
	// A tenant operator still cannot name another tenant: its ?tenant= is
	// ignored, as before.
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_users?tenant=newco", `{"subject":"sneak"}`); code != http.StatusCreated || b["tenant_id"] != "acme" {
		t.Fatalf("tenant operator: %d %v", code, b)
	}
}

// Deleting a user removes its logins and password, so a login never maps to a
// user that is gone and a recreated subject inherits nothing.
func TestUsersDelete_RemovesTheUsersLoginsAndPassword(t *testing.T) {
	f := newGateFixture(t)
	f.addUser(t, "acme", "dave", "isolated")
	f.addLogin(t, "acme", "dave", "local", "dave")
	if err := f.st.UserCredentialSet(context.Background(), store.UserCredentialRow{TenantID: "acme", Subject: "dave", Kind: store.CredentialKindPassword, PHC: "x", PepperKID: "pw1:x", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.call(t, bAcmeOp, "DELETE", "/v1/_users/dave", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if _, err := f.st.UserIdentityGet(context.Background(), "local", "", "dave"); !isNotFound(err) {
		t.Fatalf("the login survived its user: %v", err)
	}
	if _, err := f.st.UserCredentialGet(context.Background(), "acme", "dave", store.CredentialKindPassword); !isNotFound(err) {
		t.Fatalf("the password survived its user: %v", err)
	}
}

func TestAuthgateDeleteCredentials_RemovesPasswordAndTOTPAndKeepsTheLogin(t *testing.T) {
	f := newGateFixture(t)
	f.addUser(t, "acme", "erin", "isolated")
	f.addLogin(t, "acme", "erin", "local", "erin")
	ctx := context.Background()
	_ = f.st.UserCredentialSet(ctx, store.UserCredentialRow{TenantID: "acme", Subject: "erin", Kind: store.CredentialKindPassword, PHC: "x", PepperKID: "pw1:x", UpdatedAt: time.Now()})
	_ = f.st.UserTOTPPut(ctx, store.UserTOTPRow{TenantID: "acme", Subject: "erin", Sealed: "{}", CreatedAt: time.Now(), ConfirmedAt: time.Now()})
	if code, _ := f.call(t, bAcmeOp, "DELETE", "/v1/_authgate/local/credentials/erin", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if _, err := f.st.UserCredentialGet(ctx, "acme", "erin", store.CredentialKindPassword); !isNotFound(err) {
		t.Fatalf("password kept: %v", err)
	}
	if _, err := f.st.UserTOTPGet(ctx, "acme", "erin"); !isNotFound(err) {
		t.Fatalf("totp kept: %v", err)
	}
	if _, err := f.st.UserIdentityGet(ctx, "local", "", "erin"); err != nil {
		t.Fatalf("login removed: %v", err)
	}
	if got := f.audit.actions(); len(got) == 0 || got[len(got)-1] != "auth_credential_removed" {
		t.Fatalf("audit = %v", got)
	}
}

func TestValidReturnTo_RefusesForeignAndProtocolRelativeTargets(t *testing.T) {
	g := PasswordGateSettings{AllowedReturnTo: []string{"https://site.example/welcome"}}
	for _, ok := range []string{"", "/ui/", "/ui/run?x=1", "https://site.example/welcome"} {
		if !validReturnTo(g, ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"https://evil.example/", "//evil.example/", "/\\evil.example", "https://site.example/welcome/../x", "javascript:alert(1)", "/ui\r\nX: y"} {
		if validReturnTo(g, bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
