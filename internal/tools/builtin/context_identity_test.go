package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// recordingStore records every directory read the identity surface makes, so
// a test can hold that each one is keyed by the run's own identity.
type recordingStore struct {
	store.Store
	mu    sync.Mutex
	users []string // "tenant/subject"
	group []string // "tenant/name"
}

func (r *recordingStore) UserGet(ctx context.Context, tenant, subject string) (store.UserRow, error) {
	r.mu.Lock()
	r.users = append(r.users, tenant+"/"+subject)
	r.mu.Unlock()
	return r.Store.UserGet(ctx, tenant, subject)
}

func (r *recordingStore) GroupGet(ctx context.Context, tenant, name string) (store.GroupRow, error) {
	r.mu.Lock()
	r.group = append(r.group, tenant+"/"+name)
	r.mu.Unlock()
	return r.Store.GroupGet(ctx, tenant, name)
}

// identityStore seeds two tenants: in acme, alice (pending) is in support,
// which staff contains; support-tier-1 is bound to support and no-shell to
// the whole tenant. globex has its own alice and bob, with display names that
// must never surface in an acme run.
func identityStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	created := time.Date(2026, 10, 1, 9, 12, 0, 0, time.UTC)
	must(st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", DisplayName: "Alice Doe", AccessMode: "tenant", Status: store.UserStatusPending, CreatedAt: created}))
	must(st.UserCreate(ctx, store.UserRow{TenantID: "globex", Subject: "alice", DisplayName: "Globex Alice", AccessMode: "isolated", Status: "active"}))
	must(st.UserCreate(ctx, store.UserRow{TenantID: "globex", Subject: "bob", DisplayName: "Globex Bob", AccessMode: "tenant", Status: "active"}))
	_, err = st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "support", Description: "Tier 1", ExternalID: "google:support"})
	must(err)
	_, err = st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "staff"})
	must(err)
	_, err = st.GroupPut(ctx, store.GroupRow{TenantID: "globex", Name: "support", Description: "GLOBEX GROUP"})
	must(err)
	must(st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "staff", MemberKind: "group", MemberID: "support"}))
	must(st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "user", MemberID: "alice"}))
	_, err = st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "support-tier-1", Rules: json.RawMessage(
		`[{"effect":"allow","actions":["run"],"resources":["agent:chat/*","agent:support/**"]},{"effect":"deny","actions":["run"],"resources":["agent:admin/**"]}]`)})
	must(err)
	_, err = st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-shell", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)})
	must(err)
	_, err = st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "support-tier-1", SubjectKind: "group", SubjectID: "support"})
	must(err)
	_, err = st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-shell", SubjectKind: "tenant"})
	must(err)
	return st
}

// identityRun is a run of subject in acme, started by that subject's member
// token, under mode: the run identity, the principal, and the compiled set on
// the run policy, as the run entry points stamp them.
func identityRun(t *testing.T, st store.Store, mode authz.Mode, subject string) context.Context {
	t.Helper()
	p, err := aclres.New(st, mode, 0).Resolve(context.Background(), auth.Principal{
		TenantID: "acme", Subject: subject, TokenDefID: "def_" + subject, TokenSuffix: "abc123",
		Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithTransport(authz.WithMode(context.Background(), mode), "http")
	ctx = auth.WithPrincipal(ctx, p)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "acme", UserID: subject, AgentID: "a_1"})
	ctx = tools.WithRunID(ctx, "r_identity")
	return tools.WithRunPolicy(ctx, tools.RunPolicy{ACL: p.Effective, ACLRemovedTools: []string{"Bash"}})
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestContextSelf_IdentityReportsTheRunsUserGroupsAndAppliedLists(t *testing.T) {
	st := identityStore(t)
	out, res := callContext(t, &Context{Store: st}, identityRun(t, st, authz.ModeCompat, "alice"), `{"op":"self"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	id, ok := out["identity"].(map[string]any)
	if !ok {
		t.Fatalf("no identity block: %v", out)
	}
	for k, want := range map[string]any{
		"tenant_id": "acme", "subject": "alice", "display_name": "Alice Doe",
		"access_mode": "tenant", "status": "pending", "created_at": "2026-10-01T09:12:00Z",
	} {
		if id[k] != want {
			t.Errorf("identity.%s = %v, want %v", k, id[k], want)
		}
	}
	if note, _ := id["status_note"].(string); !strings.Contains(note, "approve") {
		t.Errorf("a pending account carries no note telling the agent not to work for it: %q", note)
	}
	wantGroups := `[{"name":"staff","via":"support"},{"description":"Tier 1","external_id":"google:support","name":"support","via":"direct"}]`
	if got := asJSON(t, id["groups"]); got != wantGroups {
		t.Errorf("groups = %s\nwant      %s", got, wantGroups)
	}
	acl, _ := id["acl"].(map[string]any)
	if acl["mode"] != "compat" || acl["baseline"] != authz.BaselineMember {
		t.Errorf("acl mode/baseline = %v / %v", acl["mode"], acl["baseline"])
	}
	wantLists := `[{"bound_to":{"id":"support","kind":"group"},"name":"support-tier-1","version":1},{"bound_to":{"kind":"tenant"},"name":"no-shell","version":1}]`
	if got := asJSON(t, acl["lists"]); got != wantLists {
		t.Errorf("acl.lists = %s\nwant          %s", got, wantLists)
	}
	if got := asJSON(t, acl["missing_acls"]); got != `[]` {
		t.Errorf("acl.missing_acls = %s", got)
	}
	if got := asJSON(t, acl["removed_tools"]); got != `["Bash"]` {
		t.Errorf("acl.removed_tools = %s", got)
	}
	if strings.Contains(res.Text, "Globex") || strings.Contains(res.Text, "GLOBEX") {
		t.Errorf("op=self carries another tenant's directory data: %s", res.Text)
	}
	// The flat principal block keeps exactly the keys its existing readers
	// know.
	principal, _ := out["principal"].(map[string]any)
	keys := make([]string, 0, len(principal))
	for k := range principal {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"is_admin", "legacy", "scopes", "subject", "tenant_id", "token_def_id", "token_suffix"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("principal keys = %v, want %v", keys, want)
	}
}

// A subject with no user row in the run's tenant reads as having none, even
// when a row of the same subject exists in another tenant.
func TestContextSelf_IdentityOfASubjectWithNoRowInTheRunsTenantHasNoRowFields(t *testing.T) {
	st := identityStore(t)
	out, res := callContext(t, &Context{Store: st}, identityRun(t, st, authz.ModeCompat, "bob"), `{"op":"self"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	id, _ := out["identity"].(map[string]any)
	if id["subject"] != "bob" || id["tenant_id"] != "acme" {
		t.Fatalf("identity = %v", id)
	}
	for _, k := range []string{"display_name", "status", "access_mode", "created_at"} {
		if _, ok := id[k]; ok {
			t.Errorf("identity.%s present for a subject with no row in the run's tenant: %v", k, id[k])
		}
	}
	if strings.Contains(res.Text, "Globex Bob") {
		t.Errorf("another tenant's row surfaced: %s", res.Text)
	}
}

// Open mode (no principal, no set): no identity block; the flat ids remain.
func TestContextSelf_NoIdentityBlockWithoutASubject(t *testing.T) {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "u1"})
	out, _ := callContext(t, &Context{}, ctx, `{"op":"self"}`)
	if _, ok := out["identity"]; ok {
		t.Errorf("identity block in open mode: %v", out["identity"])
	}
}

// Nothing the model sends selects whose directory rows are read: every user
// and group read is the run's own tenant, and the user read is the run's own
// subject, whatever subject or tenant the call carries.
func TestContextIdentity_ReadsAreKeyedByTheRunsOwnIdentity(t *testing.T) {
	inner := identityStore(t)
	rec := &recordingStore{Store: inner}
	ctx := identityRun(t, inner, authz.ModeCompat, "alice")
	c := &Context{Store: rec}
	for _, call := range []string{
		`{"op":"self"}`,
		`{"op":"can","subject":"bob","tenant_id":"globex","action":"run","resource":{"type":"agent","id":"admin/x"}}`,
		`{"op":"reach","subject":"bob","tenant_id":"globex","type":"agent","action":"run"}`,
	} {
		out, res := callContext(t, c, ctx, call)
		if res.IsError {
			t.Fatalf("%s: %s", call, res.Text)
		}
		if strings.Contains(call, `"op":"can"`) && (out["outcome"] != "deny" || !strings.Contains(asJSON(t, out["reasons"]), "support-tier-1")) {
			t.Errorf("can answered for someone other than the run's subject: %v", out)
		}
	}
	if len(rec.users) == 0 {
		t.Fatal("no user read recorded; the check asserts nothing")
	}
	for _, u := range rec.users {
		if u != "acme/alice" {
			t.Errorf("user read keyed %q, want acme/alice", u)
		}
	}
	for _, g := range rec.group {
		if !strings.HasPrefix(g, "acme/") {
			t.Errorf("group read keyed %q, outside the run's tenant", g)
		}
	}
}

// The identity file's store calls take no argument derived from the model's
// input: a census over the source, so a later op that wires `in.<field>` into
// a directory read fails here before it ships.
func TestContextIdentity_NoModelInputReachesAStoreCall(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "context_identity.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.SelectorExpr)
		if !ok || recv.Sel.Name != "Store" {
			return true
		}
		calls++
		for _, arg := range call.Args {
			ast.Inspect(arg, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && (id.Name == "in" || id.Name == "q" || id.Name == "queries") {
					t.Errorf("%s: c.Store.%s takes model input (%s)", fset.Position(call.Pos()), sel.Sel.Name, id.Name)
				}
				return true
			})
		}
		return true
	})
	if calls == 0 {
		t.Fatal("no store call found; the census asserts nothing")
	}
}

func TestContextCan_AnswersFromTheRunsRules(t *testing.T) {
	st := identityStore(t)
	c := &Context{Store: st}
	compat := identityRun(t, st, authz.ModeCompat, "alice")
	for _, tc := range []struct {
		call     string
		decision bool
		outcome  string
		allowed  bool
		fallback bool
	}{
		{`{"op":"can","action":"run","resource":{"type":"agent","id":"support/l2"}}`, true, "allow", true, false},
		{`{"op":"can","action":"run","resource":{"type":"agent","id":"admin/triage"}}`, false, "deny", false, false},
		{`{"op":"can","action":"invoke","resource":{"type":"tool","id":"Bash"}}`, false, "deny", false, false},
		// No rule speaks: in compat the scope checks decide.
		{`{"op":"can","action":"mint","resource":{"type":"principal","id":"user:bob"}}`, false, "no_match", true, true},
	} {
		out, res := callContext(t, c, compat, tc.call)
		if res.IsError {
			t.Fatalf("%s: %s", tc.call, res.Text)
		}
		if out["decision"] != tc.decision || out["outcome"] != tc.outcome || out["allowed"] != tc.allowed || out["mode"] != "compat" {
			t.Errorf("%s = %v", tc.call, out)
		}
		if _, ok := out["fallback"]; ok != tc.fallback {
			t.Errorf("%s: fallback present %v, want %v (%v)", tc.call, ok, tc.fallback, out)
		}
	}
	// A deny names every rule that matched, the deciding deny first.
	out, _ := callContext(t, c, compat, `{"op":"can","action":"run","resource":{"type":"agent","id":"admin/triage"}}`)
	if got := asJSON(t, out["reasons"]); got != `[{"acl":"support-tier-1","effect":"deny","rule":1,"version":1},{"acl":"builtin:member","effect":"allow","rule":0}]` {
		t.Errorf("reasons = %s", got)
	}
	// Enforce: an unmatched question is refused, and there is no fallback.
	enforce := identityRun(t, st, authz.ModeEnforce, "alice")
	out, _ = callContext(t, c, enforce, `{"op":"can","action":"mint","resource":{"type":"principal","id":"user:bob"}}`)
	if out["allowed"] != false || out["mode"] != "enforce" {
		t.Errorf("enforce no_match = %v", out)
	}
	if _, ok := out["fallback"]; ok {
		t.Errorf("enforce no_match carries a fallback: %v", out)
	}
	// Batch: answers in order.
	out, res := callContext(t, c, compat, `{"op":"can","queries":[
		{"action":"run","resource":{"type":"agent","id":"chat/x"}},
		{"action":"run","resource":{"type":"agent","id":"admin/x"}},
		{"action":"read","resource":{"type":"memory","id":"tenant"}}]}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	evs, _ := out["evaluations"].([]any)
	if len(evs) != 3 {
		t.Fatalf("evaluations = %v", out)
	}
	for i, want := range []string{"allow", "deny", "allow"} {
		if got := evs[i].(map[string]any)["outcome"]; got != want {
			t.Errorf("evaluations[%d].outcome = %v, want %s", i, got, want)
		}
	}
}

func TestContextCan_RefusesAMalformedQuestionBeforeEvaluating(t *testing.T) {
	st := identityStore(t)
	c := &Context{Store: st}
	ctx := identityRun(t, st, authz.ModeCompat, "alice")
	many := make([]string, maxCanQueries+1)
	for i := range many {
		many[i] = `{"action":"run","resource":{"type":"agent","id":"chat"}}`
	}
	for _, tc := range []struct{ call, want string }{
		{`{"op":"can"}`, "missing action and resource"},
		{`{"op":"can","action":"run"}`, "resource.type are required"},
		{`{"op":"can","action":"run","resource":{"type":"planet","id":"x"}}`, `"planet" is not a resource type`},
		{`{"op":"can","action":"read","resource":{"type":"tool","id":"Read"}}`, `"read" is not an action on tool`},
		{`{"op":"can","action":"run","resource":{"type":"agent","id":"a/../b"}}`, "climbs"},
		{`{"op":"can","action":"read","resource":{"type":"run","id":"everyone"}}`, "not one of"},
		{`{"op":"can","action":"run","resource":{"type":"agent"},"queries":[{"action":"run","resource":{"type":"agent"}}]}`, "not both"},
		{`{"op":"can","queries":[{"action":"run","resource":{"type":"agent"}},{"action":"fly","resource":{"type":"agent"}}]}`, "queries[1]"},
		{`{"op":"can","queries":[` + strings.Join(many, ",") + `]}`, fmt.Sprintf("the limit is %d", maxCanQueries)},
	} {
		_, res := callContext(t, c, ctx, tc.call)
		if !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%s = %q, want an error containing %q", tc.call, res.Text, tc.want)
		}
	}
	// A run with no compiled set: a well-formed question is refused as such,
	// a malformed one still as malformed.
	bare := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "u1"})
	if _, res := callContext(t, c, bare, `{"op":"can","action":"run","resource":{"type":"agent","id":"x"}}`); !res.IsError || !strings.Contains(res.Text, "not active") {
		t.Errorf("no set: %q", res.Text)
	}
	if _, res := callContext(t, c, bare, `{"op":"can","action":"fly","resource":{"type":"agent"}}`); !res.IsError || !strings.Contains(res.Text, "not an action") {
		t.Errorf("no set, malformed: %q", res.Text)
	}
}

func TestContextReach_ListsTheRunsAllowAndDenyPatterns(t *testing.T) {
	st := identityStore(t)
	c := &Context{Store: st}
	out, res := callContext(t, c, identityRun(t, st, authz.ModeCompat, "alice"), `{"op":"reach","type":"agent","action":"run"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	allow := asJSON(t, out["allow"])
	for _, p := range []string{`"chat/*"`, `"support/**"`, `"*"`} { // "*" is the member baseline's
		if !strings.Contains(allow, p) {
			t.Errorf("allow = %s, missing %s", allow, p)
		}
	}
	if got := asJSON(t, out["deny"]); got != `["admin/**"]` {
		t.Errorf("deny = %s", got)
	}
	if out["semantics"] != "deny_overrides" || out["unmatched"] != "scopes" || out["see_also"] == nil {
		t.Errorf("reach = %v", out)
	}
	out, _ = callContext(t, c, identityRun(t, st, authz.ModeEnforce, "alice"), `{"op":"reach","type":"tool","action":"invoke"}`)
	if got := asJSON(t, out["deny"]); got != `["Bash"]` {
		t.Errorf("tool deny = %s", got)
	}
	if got := asJSON(t, out["removed_tools"]); got != `["Bash"]` || out["unmatched"] != "denied" {
		t.Errorf("tool reach = %v", out)
	}
	for _, tc := range []struct{ call, want string }{
		{`{"op":"reach","type":"agent"}`, "type and action are required"},
		{`{"op":"reach","type":"planet","action":"run"}`, "not a resource type"},
		{`{"op":"reach","type":"tool","action":"run"}`, "not an action on tool"},
	} {
		if _, res := callContext(t, c, identityRun(t, st, authz.ModeCompat, "alice"), tc.call); !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%s = %q, want %q", tc.call, res.Text, tc.want)
		}
	}
}
