package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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

// A delegated token mint is refused when its audit record cannot be
// written, and no token is minted; a retirement goes ahead (a compromised
// token stays revocable in an audit outage) and is counted unrecorded.
func TestUserTokens_AMintIsRefusedAndARetirementCountedWhenUnrecorded(t *testing.T) {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("a retirement with the audit sink failing: %d %s; want it done", rec.Code, rec.Body.String())
	}
	if row, err := st.OperatorTokenDefGet(ctx, minted.DefID); err != nil || row.RetiredAt.IsZero() {
		t.Errorf("the token after the retirement: %+v, %v; want it retired", row.RetiredAt, err)
	}
	wantUnrecorded(t, s, "retire", 1)
}

// wantUnrecorded checks loomcycle_audit_unrecorded_total{action}.
func wantUnrecorded(t *testing.T, s *Server, action string, n int64) {
	t.Helper()
	var b strings.Builder
	s.writeAuditUnrecordedMetric(&b, "")
	want := fmt.Sprintf(`loomcycle_audit_unrecorded_total{action="%s"} %d`, action, n)
	if !strings.Contains(b.String(), want) {
		t.Errorf("metric:\n%s\nwant %s", b.String(), want)
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

// In an audit outage a user create and a re-enable are refused and change
// nothing; a disable and a delete go ahead and are counted unrecorded.
func TestUserLifecycle_GrantsAreRefusedAndReductionsCountedWhenUnrecorded(t *testing.T) {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("a disable with the audit sink failing: %d %s; want it done", rec.Code, rec.Body.String())
	}
	if u, err := st.UserGet(ctx, "acme", "alice"); err != nil || u.Status != "disabled" {
		t.Errorf("alice after the disable: %+v, %v; want disabled", u.Status, err)
	}
	wantUnrecorded(t, s, "user_disabled", 1)

	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/alice", `{"status":"active"}`, acme, "subject", "alice"))
	wantAuditUnavailable(t, "a re-enable", rec.Code, rec.Body.String())
	if u, err := st.UserGet(ctx, "acme", "alice"); err != nil || u.Status != "disabled" {
		t.Errorf("alice after the refused re-enable: %+v, %v; want disabled", u.Status, err)
	}

	rec = httptest.NewRecorder()
	s.handleDeleteUser(rec, bodyReq("DELETE", "/v1/_users/alice", "", acme, "subject", "alice"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a delete with the audit sink failing: %d %s; want it done", rec.Code, rec.Body.String())
	}
	if _, err := st.UserGet(ctx, "acme", "alice"); !isNotFound(err) {
		t.Errorf("alice after the delete: %v; want her gone", err)
	}
	wantUnrecorded(t, s, "user_deleted", 1)
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

// In an audit outage a provider's create is refused with 503 — which a SCIM
// provider retries — and leaves nothing; a deprovisioning goes ahead and is
// counted unrecorded.
func TestSCIM_ACreateIsRefusedAndADeprovisionCountedWhenUnrecorded(t *testing.T) {
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
	if code, out := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("a delete with the audit sink failing = %d %v, want it done", code, out)
	}
	if st := f.status(t, "acme", sub); st != "disabled" {
		t.Errorf("alice after the delete is %s, want disabled", st)
	}
	wantUnrecorded(t, f.srv, "auth_scim_user_deactivated", 1)
}

// Every action a change records ahead is classified, and the reductions are
// exactly the changes that only take access away. A census over this
// package's sources: a new intent call with an unclassified action fails
// here, rather than be treated as granting at run time.
func TestAuditIntents_EveryActionIsClassified(t *testing.T) {
	reduces := map[string]bool{"retire": true, "user_disabled": true, "user_deleted": true, "auth_scim_user_deactivated": true}
	for _, a := range audit.ChangeActions() {
		k, _ := audit.ChangeKindOf(a)
		if (k == audit.ChangeReduces) != reduces[a] {
			t.Errorf("%s is classed %v; the reductions are exactly %v", a, k, reduces)
		}
	}
	pattern := regexp.MustCompile(`(?:tokenIntent\(\w+, |aclIntent\(ctx, \w+, |scimIntent\(ctx, c, |recordIntent\(r\.Context\(\), audit\.Event\{Action: |Action: )"([^"]+)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, "Intent(") && !strings.Contains(line, "want := audit.Event{") && !strings.Contains(line, "want.Action = ") {
				continue
			}
			for _, m := range pattern.FindAllStringSubmatch(line, -1) {
				seen++
				if _, ok := audit.ChangeKindOf(m[1]); !ok {
					t.Errorf("%s: the action %q is recorded ahead but not classified (audit.ChangeKindOf)", f, m[1])
				}
			}
			if m := regexp.MustCompile(`want\.Action = "([^"]+)"`).FindStringSubmatch(line); m != nil {
				seen++
				if _, ok := audit.ChangeKindOf(m[1]); !ok {
					t.Errorf("%s: the action %q is recorded ahead but not classified", f, m[1])
				}
			}
		}
	}
	if seen < 15 {
		t.Errorf("the census found %d intent calls; it no longer reads them", seen)
	}
}

// A SCIM change recorded ahead that then does not happen is marked failed:
// a second create of one userName (409) leaves an intent and a failed row,
// never a bare intent that reads as a user created.
func TestSCIM_ARefusedChangeIsRecordedAsFailed(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, out := f.call(t, bSCIMAcme, "POST", "/scim/v2/Users", jsonBody(t, map[string]any{"userName": "alice@acme.test"})); code != http.StatusConflict {
		t.Fatalf("a second create = %d %v, want 409", code, out)
	}
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	var statuses []string
	for _, ev := range f.audit.evs {
		if ev.Action == "auth_scim_user_created" {
			statuses = append(statuses, ev.ChangeStatus)
		}
	}
	// The first create: intent, then the after-the-fact row; the second:
	// intent, then failed.
	if got := strings.Join(statuses, ","); got != "intent,,intent,failed" {
		t.Errorf("auth_scim_user_created rows = %q, want \"intent,,intent,failed\"", got)
	}
}
