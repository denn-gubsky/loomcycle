package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
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

// A disable that also raises the access mode is not a reduction: in an audit
// outage it is refused like any grant, and the user keeps its mode and
// status. A disable that lowers it (or leaves it) still goes ahead.
func TestUserLifecycle_DisablePlusAWideningIsRefusedUnrecorded(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedUser(t, st, "acme", "bob", "tenant", "active")
	s.tokenAudit = failingSink{errAuditDown}

	rec := httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/alice", `{"status":"disabled","access_mode":"tenant"}`, acme, "subject", "alice"))
	wantAuditUnavailable(t, "a disable that raises the access mode", rec.Code, rec.Body.String())
	if u, err := st.UserGet(ctx, "acme", "alice"); err != nil || u.Status != "active" || u.AccessMode != "isolated" {
		t.Errorf("alice after the refused patch: %+v, %v; want active and isolated", u, err)
	}
	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/alice", `{"status":"disabled","display_name":"Mallory"}`, acme, "subject", "alice"))
	wantAuditUnavailable(t, "a disable that renames", rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/bob", `{"status":"disabled","access_mode":"isolated"}`, acme, "subject", "bob"))
	if rec.Code != http.StatusOK {
		t.Fatalf("a disable that lowers the access mode: %d %s; want it done", rec.Code, rec.Body.String())
	}
	wantUnrecorded(t, s, "user_disabled", 1)
}

// patchOnlyDisables decides over every field of a user patch; a new field
// must be classified there before this list is extended.
func TestUserPatch_EveryFieldIsClassified(t *testing.T) {
	var got []string
	rt := reflect.TypeOf(updateUserRequest{})
	for i := 0; i < rt.NumField(); i++ {
		got = append(got, rt.Field(i).Name)
	}
	if strings.Join(got, ",") != "DisplayName,AccessMode,Status" {
		t.Errorf("updateUserRequest fields = %v: classify the new one in patchOnlyDisables (a field that can widen access makes a disable a grant)", got)
	}
}

// intentExempt are the functions of this package that change users, groups,
// bindings, lists or tokens without recording ahead, each for a documented
// reason (SECURITY.md, "Best-effort, by design"). Anything else that calls
// one of those store writes must record an intent in the same function.
var intentExempt = map[string]string{
	"authgate_external.go/retireGateTokensBeyond": "retirements a gate's sign-in makes (reductions inside a sign-in)",
	"authgate_identity.go/joinFloorGroups":        "a sign-in's provisioning",
	"authgate_identity.go/provision":              "a sign-in's provisioning",
	"authgate_identity.go/syncGroupsWith":         "a sign-in's group sync",
	"authgate_lifecycle.go/rejoinFloorGroups":     "a stage of a re-enable recorded by its caller",
	"authgate_lifecycle.go/stripRemovedUser":      "a removal stage: takes access away",
	"authgate_lifecycle.go/unbindSubject":         "a removal stage: takes access away",
	"authgate_password.go/joinInviteGroups":       "an invite's redemption (a sign-in)",
	"authgate_tenant_policy.go/applyTenantPolicy": "retirements enforcing a recorded tenant policy",
	"oauth_server.go/retireAppToken":              "ending an app sign-in",
	"users_tokens.go/retireDelegatedUserTokens":   "retirements inside a recorded disable, delete or removal",
	"users_tokens.go/retireUserTokensBeyond":      "retirements inside a recorded mint",
}

// TestAuditIntents_EveryStoreWriteSitsBehindAnIntent: a census of this
// package's calls to the store writes that change who may do what (users,
// groups and memberships, lists, bindings, tokens). Each enclosing function
// records an intent (…Intent( in its body) or is exempt above with a reason;
// an exemption whose function no longer writes is stale.
func TestAuditIntents_EveryStoreWriteSitsBehindAnIntent(t *testing.T) {
	writes := regexp.MustCompile(`\.store\.(UserUpdate|UserCreate|UserCreateOwner|UserDelete|GroupMemberAdd|GroupMemberRemove|GroupPut|GroupCreate|GroupDelete|AclDefPut|AclDefRetire|AclBindingPut|AclBindingRetire|OperatorTokenDefCreate|OperatorTokenDefSetRetiredAt)\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	writers := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
			if !writes.MatchString(body) {
				continue
			}
			writers++
			key := f + "/" + fd.Name.Name
			seen[key] = true
			if _, ok := intentExempt[key]; ok || strings.Contains(body, "Intent(") {
				continue
			}
			t.Errorf("%s writes users, groups, lists, bindings or tokens without recording an intent first (recordIntent), and is not exempt", key)
		}
	}
	for key := range intentExempt {
		if !seen[key] {
			t.Errorf("exemption %s is stale: the function no longer writes", key)
		}
	}
	if writers < 20 {
		t.Errorf("the census found %d writers; it no longer reads them", writers)
	}
}

// A snapshot restore can create users, groups, bindings and lists
// wholesale: in an audit outage it is refused before it reads the snapshot.
func TestSnapshotRestore_IsRefusedWhenItsAuditRecordCannotBeWritten(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")
	s.tokenAudit = failingSink{errAuditDown}
	_, err := s.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: []byte(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "audit_unavailable") {
		t.Fatalf("restore with the audit sink failing: %v; want audit_unavailable", err)
	}
}

// A restore that stops part-way after applying rows is recorded "partial"
// with what it applied, never "failed": an auditor sees the users it
// created. One that applied nothing is "failed".
func TestSnapshotRestore_APartWayFailureIsRecordedPartialWithCounts(t *testing.T) {
	ctx := context.Background()
	_, srcSt := tokenAuthServer(t, "legacy")
	seedUser(t, srcSt, "acme", "bob", "isolated", "active")
	_, raw, err := snapshot.Capture(ctx, srcSt, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(env["sections"], &sections); err != nil {
		t.Fatal(err)
	}
	if _, ok := sections["users"]; !ok {
		t.Fatalf("the capture has no users section: %v", sections)
	}
	// The users restore, then the acl section does not decode.
	sections["acl"] = json.RawMessage(`{"version":"1.0","lists":"not a list"}`)
	env["sections"], _ = json.Marshal(sections)
	delete(env, "checksum") // verify-if-present: the edit would fail it before anything applies
	broken, _ := json.Marshal(env)

	s, st, sink := sessionTestServer(t, "legacy")
	if _, err := s.RestoreSnapshot(ctx, connector.RestoreSnapshotRequest{RawJSON: broken}); err == nil {
		t.Fatal("the broken restore succeeded")
	}
	if _, err := st.UserGet(ctx, "acme", "bob"); err != nil {
		t.Fatalf("the restore did not apply the users before it stopped: %v", err)
	}
	ev := sink.events[len(sink.events)-1]
	if ev.Action != "snapshot_restored" || ev.ChangeStatus != audit.ChangePartial || ev.ChangeCounts["users"] != 1 {
		t.Errorf("last record = %s %q %v; want snapshot_restored partial with users=1", ev.Action, ev.ChangeStatus, ev.ChangeCounts)
	}

	// Nothing applied: failed.
	if _, err := s.RestoreSnapshot(ctx, connector.RestoreSnapshotRequest{RawJSON: []byte(`{"not":"a snapshot"}`)}); err == nil {
		t.Fatal("a non-snapshot restored")
	}
	if ev := sink.events[len(sink.events)-1]; ev.ChangeStatus != audit.ChangeFailed {
		t.Errorf("a restore that applied nothing is recorded %q, want failed", ev.ChangeStatus)
	}
}

// failRetires faults every token retirement.
type failRetires struct{ store.Store }

func (failRetires) OperatorTokenDefSetRetiredAt(context.Context, string, time.Time) error {
	return errDiskFull
}

// A disable whose status write applied but whose credential teardown then
// failed is recorded "partial" (the user is disabled), never "failed".
func TestUserLifecycle_ADisableWhoseTeardownFailsIsRecordedPartial(t *testing.T) {
	s, st, sink := sessionTestServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	mintFor(t, s, acme, "alice", "")
	s.store = failRetires{st}

	rec := httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/alice", `{"status":"disabled"}`, acme, "subject", "alice"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a disable whose teardown faults: %d %s; want 500", rec.Code, rec.Body.String())
	}
	if u, err := st.UserGet(ctx, "acme", "alice"); err != nil || u.Status != "disabled" {
		t.Fatalf("precondition: alice is disabled: %+v, %v", u.Status, err)
	}
	ev := sink.events[len(sink.events)-1]
	if ev.Action != "user_disabled" || ev.ChangeStatus != audit.ChangePartial || ev.ChangeCounts["disabled"] != 1 {
		t.Errorf("last record = %s %q %v; want user_disabled partial with disabled=1", ev.Action, ev.ChangeStatus, ev.ChangeCounts)
	}
}
