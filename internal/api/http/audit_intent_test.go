package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// errAuditDown stands in for an audit store that cannot take a row.
var errAuditDown = errors.New("audit store unavailable")

func wantAuditUnavailable(t *testing.T, what string, code int, body string) {
	t.Helper()
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "audit_unavailable") {
		t.Fatalf("%s with the audit sink failing: %d %s; want 503 audit_unavailable", what, code, body)
	}
}

// A delegated token mint and retirement are refused when their audit record
// cannot be written, and nothing changes: no token is minted, none retired.
func TestUserTokens_AreRefusedWhenTheirAuditRecordCannotBeWritten(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")

	// A live token to retire, minted while the audit store answers.
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var minted mintUserTokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)

	s.tokenAudit = failingSink{errAuditDown}
	rec = httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	wantAuditUnavailable(t, "a mint", rec.Code, rec.Body.String())
	if rows, _ := st.OperatorTokenDefListBySubject(ctx, "acme", "alice"); len(rows) != 1 {
		t.Errorf("tokens after the refused mint = %d, want the 1 minted before", len(rows))
	}

	req := bodyReq("DELETE", "/v1/_users/alice/tokens/"+minted.DefID, "", acme, "subject", "alice")
	req.SetPathValue("def_id", minted.DefID)
	rec = httptest.NewRecorder()
	s.handleRevokeUserToken(rec, req)
	wantAuditUnavailable(t, "a retirement", rec.Code, rec.Body.String())
	if row, err := st.OperatorTokenDefGet(ctx, minted.DefID); err != nil || !row.RetiredAt.IsZero() {
		t.Errorf("the token after the refused retirement: %+v, %v; want it live", row.RetiredAt, err)
	}
}

// With the audit store answering, the record is written before the change
// and says so.
func TestUserTokens_RecordTheirIntentBeforeTheChange(t *testing.T) {
	s, st, sink := sessionTestServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	ev := lastEvent(t, sink, "create")
	if ev.ChangeStatus != audit.ChangeIntent || ev.TargetSubject != "alice" || ev.TargetDefID == "" {
		t.Errorf("mint record = %+v; want an intent naming alice's new token", ev)
	}
}

// The user lifecycle — create, update (a disable), delete — is refused when
// its record cannot be written, and the user is as it was.
func TestUserLifecycle_IsRefusedWhenItsAuditRecordCannotBeWritten(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "tenant", "active")
	s.tokenAudit = failingSink{errAuditDown}

	rec := httptest.NewRecorder()
	s.handleCreateUser(rec, bodyReq("POST", "/v1/_users", `{"subject":"bob"}`, acme, "", ""))
	wantAuditUnavailable(t, "a user create", rec.Code, rec.Body.String())
	if _, err := st.UserGet(ctx, "acme", "bob"); !isNotFound(err) {
		t.Errorf("bob after the refused create: %v; want not found", err)
	}

	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/alice", `{"status":"disabled"}`, acme, "subject", "alice"))
	wantAuditUnavailable(t, "a disable", rec.Code, rec.Body.String())
	if u, err := st.UserGet(ctx, "acme", "alice"); err != nil || u.Status != "active" {
		t.Errorf("alice after the refused disable: %+v, %v; want active", u.Status, err)
	}

	rec = httptest.NewRecorder()
	s.handleDeleteUser(rec, bodyReq("DELETE", "/v1/_users/alice", "", acme, "subject", "alice"))
	wantAuditUnavailable(t, "a delete", rec.Code, rec.Body.String())
	if _, err := st.UserGet(ctx, "acme", "alice"); err != nil {
		t.Errorf("alice after the refused delete: %v; want her kept", err)
	}
}

// Access-control writes are refused on the route and on the op-dispatched
// twin the gRPC RPCs and MCP tools share, and nothing is written.
func TestACLWrites_AreRefusedWhenTheirAuditRecordCannotBeWritten(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	ctx := context.Background()
	srv.tokenAudit = failingSink{errAuditDown}

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/_groups", `{"name":"support"}`},
		{"POST", "/v1/_acldef", `{"op":"create","name":"no-bash","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]}`},
	} {
		code, body := bearerCall(t, ts, operatorBearer, c.method, c.path, c.body)
		wantAuditUnavailable(t, c.method+" "+c.path, code, body)
	}
	if _, err := st.GroupGet(ctx, "acme", "support"); !isNotFound(err) {
		t.Errorf("the group after the refused write: %v; want not found", err)
	}
	if _, err := st.AclDefGet(ctx, "acme", "no-bash"); !isNotFound(err) {
		t.Errorf("the list after the refused write: %v; want not found", err)
	}

	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	res := twinCall(t, srv, op, "groupdef", `{"op":"create","name":"support"}`)
	if !res.IsError || !strings.Contains(res.Text, "audit_unavailable") ||
		res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryTransient {
		t.Errorf("the twin with the audit sink failing: %+v; want a transient audit_unavailable", res)
	}
}

// The operator-token tool behind POST /v1/_operatortokendef, its gRPC RPC and
// its MCP tool refuses a mint it cannot record, and stores nothing.
func TestOperatorTokenDef_RefusesAMintItCannotRecord(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	hasher, err := s.cfg().Env.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	tool := &builtin.OperatorTokenDef{Store: st, Hasher: hasher, Audit: failingSink{errAuditDown}}
	ctx := tools.WithOperatorTokenDefPolicy(context.Background(), tools.OperatorTokenDefPolicyValue{Admin: true})
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"ci","tenant_id":"acme","subject":"ci","scopes":["runs:create"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.HasPrefix(res.Text, tools.AuditUnavailablePrefix) {
		t.Fatalf("create with the audit sink failing: %+v; want audit_unavailable", res)
	}
	if _, err := st.OperatorTokenDefGetCurrentByName(context.Background(), "ci"); err == nil {
		t.Error("a token was minted although its record was refused")
	}
}

// A provider's create and deactivate are refused with 503 — which a SCIM
// provider retries — when their record cannot be written, and the user is
// as it was.
func TestSCIM_UserChangesAreRefusedWhenTheirAuditRecordCannotBeWritten(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	f.srv.tokenAudit = failingSink{errAuditDown}

	code, out := f.call(t, bSCIMAcme, "POST", "/scim/v2/Users", jsonBody(t, map[string]any{"userName": "bob@acme.test"}))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("a create with the audit sink failing = %d %v, want 503", code, out)
	}
	if rows, _, _ := f.st.SCIMUserList(context.Background(), "okta", "acme", store.SCIMFilter{UserNameKey: "bob@acme.test"}, 0, 10); len(rows) != 0 {
		t.Errorf("the refused create left a resource: %+v", rows)
	}
	if code, out := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+id, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("a delete with the audit sink failing = %d %v, want 503", code, out)
	}
	if st := f.status(t, "acme", sub); st != "active" {
		t.Errorf("alice after the refused delete is %s, want active", st)
	}
}
