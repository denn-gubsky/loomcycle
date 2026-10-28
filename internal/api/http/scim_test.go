package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P6 inbound SCIM 2.0 through a kind: scim gate "okta": one tenant per
// token, the identity map's floor and trusted rule, group sync into the
// gate's groups, and the removal policy for a deprovisioned user.

const (
	bSCIMAcme  = "scim-acme-bearer"
	bSCIMOther = "scim-other-bearer"
)

type scimOpts struct {
	trusted, groups, authoritative bool
	tenant                         string
	maxRemovals                    int
	newUserStatus                  string
}

func newSCIMFixture(t *testing.T, o scimOpts) *gateFixture {
	t.Helper()
	f := newGateFixture(t)
	seedToken(t, f.st, bSCIMAcme, "acme", "idp-acme", []string{auth.ScopeSCIMProvision}, time.Time{})
	seedToken(t, f.st, bSCIMOther, "other", "idp-other", []string{auth.ScopeSCIMProvision}, time.Time{})
	status := o.newUserStatus
	if status == "" {
		status = "active"
	}
	idm := &config.AuthIdentityMap{Tenant: o.tenant, Provision: config.ProvisionAuto, Trusted: o.trusted,
		SyncGroups: o.groups, GroupsProvision: o.groups, AuthoritativeForUsers: o.authoritative,
		NewUser: config.AuthNewUser{AccessMode: "isolated", Groups: []string{"everyone"}, Status: status, TokenTTL: time.Hour}}
	cfg := f.srv.cfg()
	cfg.Env.PublicURL = "https://lc.example"
	cfg.ResolvedAuth.Gates = append(cfg.ResolvedAuth.Gates, config.AuthGate{Name: "okta", Kind: config.AuthGateKindSCIM, Label: "okta",
		Identity: idm, SCIM: &config.AuthSCIMGate{MaxSyncRemovals: o.maxRemovals, Removal: config.AuthRemovalPolicy{UnbindAfter: 168 * time.Hour}}})
	for _, tenant := range []string{"acme", "other"} {
		if _, err := f.st.GroupCreate(context.Background(), store.GroupRow{TenantID: tenant, Name: "everyone"}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// scimUser creates a user through SCIM and returns its SCIM id and subject.
func (f *gateFixture) scimUser(t *testing.T, bearer, userName string) (string, string) {
	t.Helper()
	code, body := f.call(t, bearer, "POST", "/scim/v2/Users", jsonBody(t, map[string]any{
		"schemas": []string{scimSchemaUser}, "userName": userName, "name": map[string]string{"givenName": "Al", "familyName": "Ice"},
		"emails": []map[string]any{{"value": userName, "primary": true}}}))
	if code != http.StatusCreated {
		t.Fatalf("create %s: %d %v", userName, code, body)
	}
	id := body["id"].(string)
	return id, "okta:" + id
}

func scimPatchBody(t *testing.T, ops ...map[string]any) string {
	return jsonBody(t, map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": ops})
}

func (f *gateFixture) status(t *testing.T, tenant, subject string) string {
	t.Helper()
	u, err := f.st.UserGet(context.Background(), tenant, subject)
	if err != nil {
		t.Fatalf("user %s: %v", subject, err)
	}
	return u.Status
}

func TestSCIM_ProvisionsAUserThroughTheGatesIdentityMap(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	id, subject := f.scimUser(t, bSCIMAcme, "Alice@Acme.test")
	ctx := context.Background()
	u, err := f.st.UserGet(ctx, "acme", subject)
	if err != nil || u.AccessMode != "isolated" || u.Status != "active" || u.DisplayName != "Al Ice" {
		t.Fatalf("the user: %+v %v", u, err)
	}
	if groups, _ := f.st.GroupsOfSubject(ctx, "acme", subject); !slices.Equal(groups, []string{"everyone"}) {
		t.Fatalf("floor groups = %v", groups)
	}
	if row, err := f.st.UserIdentityGet(ctx, "okta", "", id); err != nil || row.Subject != subject || row.TenantID != "acme" {
		t.Fatalf("the gate identity: %+v %v", row, err)
	}
	code, body := f.call(t, bSCIMAcme, "GET", `/scim/v2/Users?filter=userName%20eq%20%22alice@acme.test%22`, "")
	if code != http.StatusOK || body["totalResults"].(float64) != 1 {
		t.Fatalf("filter: %d %v", code, body)
	}
	if code, body := f.call(t, bSCIMAcme, "GET", "/scim/v2/Users/"+id, ""); code != http.StatusOK || body["userName"] != "Alice@Acme.test" || body["active"] != true {
		t.Fatalf("get: %d %v", code, body)
	}
	if code, _ := f.call(t, bSCIMAcme, "POST", "/scim/v2/Users", `{"userName":"alice@acme.TEST"}`); code != http.StatusConflict {
		t.Fatalf("duplicate userName = %d, want 409", code)
	}
	for _, q := range []string{`filter=title%20eq%20%22x%22`, `filter=userName%20co%20%22a%22`} {
		if code, _ := f.call(t, bSCIMAcme, "GET", "/scim/v2/Users?"+q, ""); code != http.StatusBadRequest {
			t.Errorf("unsupported filter %s = %d, want 400", q, code)
		}
	}
}

// A SCIM token acts in its own token's tenant only: another tenant's token
// sees and changes none of its users, a token of the shared tenant is
// refused, and a gate fixed to one tenant serves no other.
func TestSCIM_IsConfinedToItsTokensTenant(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	id, subject := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	for method, body := range map[string]string{"GET": "", "PUT": `{"userName":"x","active":false}`, "DELETE": ""} {
		if code, _ := f.call(t, bSCIMOther, method, "/scim/v2/Users/"+id, body); code != http.StatusNotFound {
			t.Errorf("other tenant %s = %d, want 404", method, code)
		}
	}
	if code, body := f.call(t, bSCIMOther, "GET", "/scim/v2/Users", ""); code != http.StatusOK || body["totalResults"].(float64) != 0 {
		t.Fatalf("other tenant list: %d %v", code, body)
	}
	if f.status(t, "acme", subject) != "active" {
		t.Fatal("another tenant's token changed the user")
	}
	// A credential of the shared tenant (a declared principal can name none)
	// is refused.
	r := httptest.NewRequest("GET", "/scim/v2/Users", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Subject: "idp", Scopes: []string{auth.ScopeSCIMProvision}}))
	rec := httptest.NewRecorder()
	if _, ok := f.srv.scimCaller(rec, r); ok || rec.Code != http.StatusForbidden {
		t.Fatalf("shared-tenant credential admitted: %d", rec.Code)
	}
	g := f.srv.cfg().ResolvedAuth.Gates
	g[len(g)-1].Identity.Tenant = "acme"
	if code, _ := f.call(t, bSCIMOther, "GET", "/scim/v2/Users", ""); code != http.StatusForbidden {
		t.Fatalf("a gate fixed to acme served other: %d", code)
	}
}

// /scim/v2 is reserved to a scim:provision token, and such a token reaches
// nothing else.
func TestSCIM_IsReservedToASCIMToken(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	for _, b := range []string{bAdmin, bAcmeOp} {
		if code, _ := f.call(t, b, "GET", "/scim/v2/Users", ""); code != http.StatusForbidden {
			t.Errorf("%s on /scim/v2 = %d, want 403", b, code)
		}
	}
	for _, path := range []string{"/v1/runs", "/v1/_users", "/v1/_me"} {
		if code, _ := f.call(t, bSCIMAcme, "GET", path, ""); code != http.StatusForbidden {
			t.Errorf("SCIM token on %s = %d, want 403", path, code)
		}
	}
}

// An untrusted gate may not assert an access mode — on create or update —
// and is refused rather than ignored; a trusted one may.
func TestSCIM_AccessModeNeedsATrustedGate(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	id, subject := f.scimUser(t, bSCIMAcme, "bob@acme.test")
	raise := scimPatchBody(t, map[string]any{"op": "replace", "path": scimSchemaExt + ":accessMode", "value": "tenant"})
	if code, body := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, raise); code != http.StatusForbidden {
		t.Fatalf("raise, untrusted = %d %v, want 403", code, body)
	}
	if code, _ := f.call(t, bSCIMAcme, "POST", "/scim/v2/Users", jsonBody(t, map[string]any{"userName": "carol@acme.test",
		scimSchemaExt: map[string]string{"accessMode": "tenant"}})); code != http.StatusForbidden {
		t.Fatalf("create at tenant, untrusted = %d, want 403", code)
	}
	if u, _ := f.st.UserGet(context.Background(), "acme", subject); u.AccessMode != "isolated" {
		t.Fatalf("access mode = %q", u.AccessMode)
	}
	tf := newSCIMFixture(t, scimOpts{trusted: true})
	id, subject = tf.scimUser(t, bSCIMAcme, "bob@acme.test")
	if code, body := tf.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, raise); code != http.StatusOK {
		t.Fatalf("raise, trusted = %d %v", code, body)
	}
	if u, _ := tf.st.UserGet(context.Background(), "acme", subject); u.AccessMode != "tenant" {
		t.Fatalf("trusted raise: access mode = %q", u.AccessMode)
	}
}

// Groups are what the provider asserts, synced into the gate's own
// "<gate>:<name>" groups — added and removed — and never a local group.
func TestSCIM_GroupMembershipSyncsIntoTheGatesGroupsBothWays(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{trusted: true, groups: true})
	ctx := context.Background()
	aliceID, alice := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	bobID, bob := f.scimUser(t, bSCIMAcme, "bob@acme.test")
	if _, err := f.st.GroupCreate(ctx, store.GroupRow{TenantID: "acme", Name: "local-ops"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "local-ops", MemberKind: store.GroupMemberUser, MemberID: alice}); err != nil {
		t.Fatal(err)
	}
	code, body := f.call(t, bSCIMAcme, "POST", "/scim/v2/Groups", jsonBody(t, map[string]any{"displayName": "Sales EMEA",
		"members": []map[string]string{{"value": aliceID}, {"value": bobID}}}))
	if code != http.StatusCreated {
		t.Fatalf("create group: %d %v", code, body)
	}
	gid := body["id"].(string)
	g, err := f.st.GroupGet(ctx, "acme", "sales-emea")
	if err != nil || g.ExternalID != "okta:sales-emea" {
		t.Fatalf("the synced group: %+v %v", g, err)
	}
	for _, sub := range []string{alice, bob} {
		if groups, _ := f.st.GroupsOfSubject(ctx, "acme", sub); !slices.Contains(groups, "sales-emea") {
			t.Fatalf("%s groups = %v", sub, groups)
		}
	}
	rm := scimPatchBody(t, map[string]any{"op": "remove", "path": `members[value eq "` + aliceID + `"]`})
	if code, body := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Groups/"+gid, rm); code != http.StatusOK {
		t.Fatalf("remove member: %d %v", code, body)
	}
	groups, _ := f.st.GroupsOfSubject(ctx, "acme", alice)
	if slices.Contains(groups, "sales-emea") || !slices.Contains(groups, "local-ops") {
		t.Fatalf("after removal alice is in %v; want sales-emea gone and local-ops kept", groups)
	}
	if code, _ := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Groups/"+gid, ""); code != http.StatusNoContent {
		t.Fatalf("delete group: %d", code)
	}
	if groups, _ := f.st.GroupsOfSubject(ctx, "acme", bob); slices.Contains(groups, "sales-emea") {
		t.Fatalf("bob kept a deleted SCIM group's membership: %v", groups)
	}
	// A member from another tenant is not a member.
	otherID, _ := f.scimUser(t, bSCIMOther, "mallory@other.test")
	if code, _ := f.call(t, bSCIMAcme, "POST", "/scim/v2/Groups", jsonBody(t, map[string]any{"displayName": "X",
		"members": []map[string]string{{"value": otherID}}})); code != http.StatusBadRequest {
		t.Fatalf("a cross-tenant member = %d, want 400", code)
	}
}

// Group push needs group sync on a trusted gate.
func TestSCIM_GroupsNeedATrustedGateWithGroupSync(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	if code, _ := f.call(t, bSCIMAcme, "POST", "/scim/v2/Groups", `{"displayName":"Admins"}`); code != http.StatusForbidden {
		t.Fatalf("group create on an untrusted gate = %d, want 403", code)
	}
}

// DELETE and active:false are the removal policy: the user is disabled and
// kept, never hard-deleted — and only by a gate authoritative for users.
func TestSCIM_DeleteDisablesAndKeepsTheUser(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	id, subject := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+id, ""); code != http.StatusForbidden {
		t.Fatalf("DELETE on a non-authoritative gate = %d, want 403", code)
	}
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": false})); code != http.StatusForbidden {
		t.Fatalf("active:false on a non-authoritative gate = %d, want 403", code)
	}
	if f.status(t, "acme", subject) != "active" {
		t.Fatal("a refused removal changed the user")
	}
	af := newSCIMFixture(t, scimOpts{authoritative: true})
	id, subject = af.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := af.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", code)
	}
	if af.status(t, "acme", subject) != "disabled" {
		t.Fatal("the deleted user is not disabled")
	}
	rm, err := af.st.AuthRemovalGet(context.Background(), "acme", subject)
	if err != nil || rm.Gate != "okta" || rm.Reason != store.RemovalReasonSCIM {
		t.Fatalf("removal = %+v %v", rm, err)
	}
	if code, _ := af.call(t, bSCIMAcme, "GET", "/scim/v2/Users/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("a deleted resource still reads: %d", code)
	}
}

// Removals are held to max_sync_removals per 24 hours.
func TestSCIM_RemovalsAreHeldToTheCeiling(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true, maxRemovals: 2})
	var ids []string
	for _, n := range []string{"a@acme.test", "b@acme.test", "c@acme.test"} {
		id, _ := f.scimUser(t, bSCIMAcme, n)
		ids = append(ids, id)
	}
	for i, id := range ids {
		code, _ := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+id, "")
		want := http.StatusNoContent
		if i == 2 {
			want = http.StatusTooManyRequests
		}
		if code != want {
			t.Fatalf("removal %d = %d, want %d", i+1, code, want)
		}
	}
	if f.status(t, "acme", "okta:"+ids[2]) != "active" {
		t.Fatal("the removal over the ceiling went through")
	}
}

// SCIM never approves a pending user: active:true leaves it pending, and
// the resource reads active:false until an operator approves it.
func TestSCIM_NeverMovesAPendingUserToActive(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{newUserStatus: store.UserStatusPending, authoritative: true})
	id, subject := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if f.status(t, "acme", subject) != store.UserStatusPending {
		t.Fatal("the user is not pending")
	}
	for _, body := range []string{
		scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": true}),
		scimPatchBody(t, map[string]any{"op": "replace", "value": map[string]any{"active": true}}),
	} {
		code, out := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, body)
		if code != http.StatusOK || out["active"] != false {
			t.Fatalf("active:true on a pending user: %d %v", code, out)
		}
	}
	if code, out := f.call(t, bSCIMAcme, "PUT", "/scim/v2/Users/"+id, `{"userName":"alice@acme.test","active":true}`); code != http.StatusOK || out["active"] != false {
		t.Fatalf("PUT active:true on a pending user: %d %v", code, out)
	}
	if code, _ := f.call(t, bSCIMAcme, "PUT", "/scim/v2/Users/"+id, `{"userName":"alice@acme.test"}`); code != http.StatusOK {
		t.Fatalf("PUT without active: %d", code)
	}
	if f.status(t, "acme", subject) != store.UserStatusPending {
		t.Fatalf("status = %q, want pending", f.status(t, "acme", subject))
	}
}

// SCIM re-enables only a user its own SCIM disable removed; an operator's
// disable and another gate's removal stand.
func TestSCIM_ReEnablesOnlyAUserItDisabled(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true, trusted: true})
	ctx := context.Background()
	off := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": false})
	on := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": true})
	id, subject := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, off); code != http.StatusOK || f.status(t, "acme", subject) != "disabled" {
		t.Fatalf("active:false = %d, status %s", code, f.status(t, "acme", subject))
	}
	if code, out := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, on); code != http.StatusOK || out["active"] != true || f.status(t, "acme", subject) != "active" {
		t.Fatalf("re-enable after its own disable = %d %v", code, out)
	}
	// An operator's disable (no removal) stands.
	id2, s2 := f.scimUser(t, bSCIMAcme, "bob@acme.test")
	disabled := "disabled"
	if err := f.st.UserUpdate(ctx, "acme", s2, store.UserPatch{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id2, on); code != http.StatusForbidden || f.status(t, "acme", s2) != "disabled" {
		t.Fatalf("re-enable of an operator's disable = %d, status %s", code, f.status(t, "acme", s2))
	}
	// Another gate's removal stands.
	id3, s3 := f.scimUser(t, bSCIMAcme, "carol@acme.test")
	if err := f.srv.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: time.Hour}, "acme", s3, "other-gate", store.RemovalReasonRevoked, "gate:other-gate"); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.call(t, bSCIMAcme, "PUT", "/scim/v2/Users/"+id3, `{"userName":"carol@acme.test","active":true}`); code != http.StatusForbidden || f.status(t, "acme", s3) != "disabled" {
		t.Fatalf("re-enable of another gate's removal = %d, status %s", code, f.status(t, "acme", s3))
	}
}
