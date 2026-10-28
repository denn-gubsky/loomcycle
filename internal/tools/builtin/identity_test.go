package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The directory fixture: identityStore's two tenants, plus in acme dave
// (member of support, so of staff) and erin (member of eng), both active, and
// User Profiles for dave (with a Public section and private sections) and for
// erin (with none). bob exists only in globex.
type identityFixture struct {
	st   store.Store
	mgr  *sqlmem.Manager
	sink *recordingSink
}

const daveProfile = "# User Profile\n\n## Identity\n\n- `@name` Dave Lin\n- `@alias` dl\n\n" +
	"## Public\n\n<!-- guidance -->\n\nPayments on-call. Ask me about refunds.\n\n" +
	"## Role and context\n\nPRIVATE-ROLE owns the ledger.\n\n## Standing preferences\n\nPRIVATE-PREF never push.\n"

const erinProfile = "# User Profile\n\n## Identity\n\n- `@name` Erin Ko\n\n## Standing preferences\n\nPRIVATE-PREF terse.\n"

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	st := identityStore(t)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "dave", DisplayName: "Dave Lin", AccessMode: "tenant", Status: "active"}))
	must(st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "erin", DisplayName: "Erin Ko", AccessMode: "tenant", Status: "active"}))
	_, err = st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "eng", Description: "Engineering"})
	must(err)
	must(st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "user", MemberID: "dave"}))
	must(st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "eng", MemberKind: "user", MemberID: "erin"}))
	writeProfile(t, st, mgr, "acme", "dave", daveProfile)
	writeProfile(t, st, mgr, "acme", "erin", erinProfile)
	writeProfile(t, st, mgr, "globex", "bob", "# User Profile\n\n## Public\n\nGLOBEX-PUBLIC\n")
	return &identityFixture{st: st, mgr: mgr, sink: &recordingSink{}}
}

// writeProfile files md as subject's User Profile, as that person's own
// agent would.
func writeProfile(t *testing.T, st store.Store, mgr *sqlmem.Manager, tenant, subject, md string) {
	t.Helper()
	d := &Document{Store: st, SqlMem: mgr}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: tenant, UserID: subject, AgentID: "a"})
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"user","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	pj, _ := json.Marshal(memrank.UserRootPath)
	if _, r := docExec(t, d, ctx, `{"op":"set_path","scope":"user","id":"`+out["document_id"].(string)+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
}

func (f *identityFixture) tool(mode authz.Mode) *Identity {
	return &Identity{Store: f.st, SqlMem: f.mgr, ACL: aclres.New(f.st, mode, 0), Audit: f.sink}
}

// operatorRun is a run in acme started by a tenant operator's token.
func (f *identityFixture) operatorRun(t *testing.T, mode authz.Mode) context.Context {
	t.Helper()
	p, err := aclres.New(f.st, mode, 0).Resolve(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "op1", TokenDefID: "def_op1", Scopes: []string{auth.ScopeTenant},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithTransport(authz.WithMode(context.Background(), mode), "http")
	ctx = auth.WithPrincipal(ctx, p)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "acme", UserID: "op1", AgentID: "a_op"})
	ctx = tools.WithRunID(ctx, "r_op")
	return tools.WithRunPolicy(ctx, tools.RunPolicy{ACL: p.Effective})
}

// grant binds an operator-written list to subject in acme.
func (f *identityFixture) grant(t *testing.T, list, subject, rules string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: list, Rules: json.RawMessage(rules)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: list, SubjectKind: "user", SubjectID: subject}); err != nil {
		t.Fatal(err)
	}
}

const readPrincipalsRules = `[{"effect":"allow","actions":["read"],"resources":["principal:user:*","principal:group:*"]}]`

func callIdentity(t *testing.T, i *Identity, ctx context.Context, in string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := i.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("result is not JSON: %v\n%s", err, res.Text)
		}
	}
	return out, res
}

func wantNotFound(t *testing.T, res tools.Result, what string) {
	t.Helper()
	if !res.IsError || !strings.Contains(res.Text, "identity_not_found") {
		t.Errorf("%s: want identity_not_found, got %s", what, res.Text)
	}
}

func TestIdentity_ASubjectOfAnotherTenantReadsAsNotFound(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeCompat)
	ctx := f.operatorRun(t, authz.ModeCompat)
	for _, in := range []string{
		`{"op":"profile","subject":"bob"}`,
		`{"op":"user","subject":"bob"}`,
		`{"op":"can","subject":"bob","action":"run","resource":{"type":"agent","id":"chat"}}`,
		`{"op":"explain","subject":"bob","action":"run","resource":{"type":"agent","id":"chat"}}`,
		`{"op":"groups","member":"bob"}`,
	} {
		_, res := callIdentity(t, i, ctx, in)
		if strings.Contains(in, `"groups"`) {
			// bob has no groups in acme: the listing is empty, never globex's.
			if res.IsError || strings.Contains(res.Text, "GLOBEX") {
				t.Errorf("%s: %s", in, res.Text)
			}
			continue
		}
		wantNotFound(t, res, in)
	}
	out, res := callIdentity(t, i, ctx, `{"op":"users"}`)
	if res.IsError || strings.Contains(res.Text, "Globex") || strings.Contains(res.Text, `"bob"`) {
		t.Errorf("users lists another tenant's people: %s", res.Text)
	}
	if n := len(out["users"].([]any)); n != 3 {
		t.Errorf("users = %d rows, want acme's 3", n)
	}
	if _, res := callIdentity(t, i, ctx, `{"op":"group","name":"support"}`); strings.Contains(res.Text, "GLOBEX") {
		t.Errorf("acme's support read globex's row: %s", res.Text)
	}
}

func TestIdentityProfile_ReturnsThePublicSectionAndNamesOnly(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeCompat)
	// A member holding the tool reads another member's profile: the tool
	// grant is the whole lock on public information.
	ctx := identityRun(t, f.st, authz.ModeCompat, "erin")
	out, res := callIdentity(t, i, ctx, `{"op":"profile","subject":"dave"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["public"] != "Payments on-call. Ask me about refunds." || out["display_name"] != "Dave Lin" ||
		!reflect.DeepEqual(out["names"], []any{"Dave Lin", "dl"}) {
		t.Errorf("profile = %v", out)
	}
	for _, private := range []string{"PRIVATE", "guidance", "ledger", "status", "groups"} {
		if strings.Contains(res.Text, private) {
			t.Errorf("profile carries %q: %s", private, res.Text)
		}
	}
	// No Public section: the names, nothing of the rest.
	out, res = callIdentity(t, i, identityRun(t, f.st, authz.ModeCompat, "dave"), `{"op":"profile","subject":"erin"}`)
	if res.IsError || strings.Contains(res.Text, "PRIVATE") || out["public"] != nil || !reflect.DeepEqual(out["names"], []any{"Erin Ko"}) {
		t.Errorf("a profile with no Public section: %s", res.Text)
	}
}

// An isolated user is sandboxed to their own scope: they do not browse the
// tenant's people, even the public profiles, unless granted the directory.
func TestIdentity_AnIsolatedUserSeesOnlyThemselves(t *testing.T) {
	f := newIdentityFixture(t)
	if err := f.st.UserUpdate(context.Background(), "acme", "erin", store.UserPatch{AccessMode: ptrString("isolated")}); err != nil {
		t.Fatal(err)
	}
	p, err := aclres.New(f.st, authz.ModeCompat, 0).Resolve(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "erin", TokenDefID: "def_erin", Scopes: []string{auth.ScopeUser},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithTransport(authz.WithMode(context.Background(), authz.ModeCompat), "http")
	ctx = auth.WithPrincipal(ctx, p)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "acme", UserID: "erin", AgentID: "a_1", Isolated: true})
	ctx = tools.WithRunPolicy(tools.WithRunID(ctx, "r_iso"), tools.RunPolicy{ACL: p.Effective})
	i := f.tool(authz.ModeCompat)
	_, res := callIdentity(t, i, ctx, `{"op":"profile","subject":"dave"}`)
	wantNotFound(t, res, "an isolated user reading another's profile")
	out, res := callIdentity(t, i, ctx, `{"op":"users"}`)
	if res.IsError || len(out["users"].([]any)) != 1 || out["users"].([]any)[0].(map[string]any)["subject"] != "erin" {
		t.Errorf("an isolated user lists %s, want only themselves", res.Text)
	}
	if out, res := callIdentity(t, i, ctx, `{"op":"profile","subject":"erin"}`); res.IsError || out["subject"] != "erin" {
		t.Errorf("an isolated user's own profile: %s", res.Text)
	}
}

func ptrString(s string) *string { return &s }

// A lookup reads, and never provisions, the other person's profile.
func TestIdentityProfile_DoesNotProvisionAMissingProfile(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeCompat)
	out, res := callIdentity(t, i, f.operatorRun(t, authz.ModeCompat), `{"op":"profile","subject":"alice"}`)
	if res.IsError || out["display_name"] != "Alice Doe" || out["public"] != nil {
		t.Fatalf("profile of a person with no profile: %s", res.Text)
	}
	parent, name, _ := splitPath(memrank.UserRootPath)
	if _, err := f.st.DirentGet(context.Background(), "acme", "user", "alice", parent, name); !isNotFound(err) {
		t.Errorf("the lookup filed a profile for alice: %v", err)
	}
}

func TestIdentityUser_AMemberReadsOnlyThemselvesAndAnOperatorReadsAnyone(t *testing.T) {
	f := newIdentityFixture(t)
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			i := f.tool(mode)
			member := identityRun(t, f.st, mode, "dave")
			out, res := callIdentity(t, i, member, `{"op":"user","subject":"dave"}`)
			if res.IsError || out["status"] != "active" || out["groups"] == nil || out["access_mode"] != "tenant" {
				t.Errorf("user{self} is not complete: %s", res.Text)
			}
			// The member baseline's own allow of `read principal:user:*` is
			// not a directory grant (it admits the operator routes, which
			// confine a member to themselves).
			_, res = callIdentity(t, i, member, `{"op":"user","subject":"erin"}`)
			wantNotFound(t, res, "a member reading another member")
			out, res = callIdentity(t, i, member, `{"op":"users"}`)
			if res.IsError {
				t.Fatal(res.Text)
			}
			for _, r := range out["users"].([]any) {
				row := r.(map[string]any)
				_, hasStatus := row["status"]
				_, hasGroups := row["groups"]
				if self := row["subject"] == "dave"; hasStatus != self || hasGroups != self {
					t.Errorf("a member's users row %v: authority shown=%v, want only on their own row", row, hasStatus)
				}
				if row["display_name"] == nil {
					t.Errorf("row %v has no display name", row)
				}
			}

			op := f.operatorRun(t, mode)
			out, res = callIdentity(t, i, op, `{"op":"user","subject":"erin"}`)
			if res.IsError || out["status"] != "active" || out["access_mode"] != "tenant" ||
				!reflect.DeepEqual(out["groups"], []any{map[string]any{"name": "eng", "via": "direct"}}) {
				t.Errorf("an operator's user{erin}: %s", res.Text)
			}
			out, _ = callIdentity(t, i, op, `{"op":"users"}`)
			for _, r := range out["users"].([]any) {
				if row := r.(map[string]any); row["status"] == nil || row["groups"] == nil {
					t.Errorf("an operator's users row lacks authority: %v", row)
				}
			}
		})
	}
}

// In enforce mode an operator-written list opens the directory to a member.
func TestIdentityUser_AnExplicitGrantOpensTheDirectoryInEnforce(t *testing.T) {
	f := newIdentityFixture(t)
	f.grant(t, "directory-read", "dave", readPrincipalsRules)
	out, res := callIdentity(t, f.tool(authz.ModeEnforce), identityRun(t, f.st, authz.ModeEnforce, "dave"), `{"op":"user","subject":"erin"}`)
	if res.IsError || out["status"] != "active" {
		t.Errorf("an explicit grant did not open user{erin}: %s", res.Text)
	}
	if _, ok := out["access_mode"]; ok {
		t.Error("read opened the manage-only field access_mode")
	}
	// compat: an allow never widens the class rule.
	_, res = callIdentity(t, f.tool(authz.ModeCompat), identityRun(t, f.st, authz.ModeCompat, "dave"), `{"op":"user","subject":"erin"}`)
	wantNotFound(t, res, "compat with an explicit grant")
}

func TestIdentityCan_RefusesAQuestionTheCallerCouldNotAnswerYesToItself(t *testing.T) {
	f := newIdentityFixture(t)
	f.grant(t, "directory-read", "dave", readPrincipalsRules)
	f.grant(t, "erin-admin", "erin", `[{"effect":"allow","actions":["run"],"resources":["agent:admin/*"]}]`)
	i := f.tool(authz.ModeEnforce)
	dave := identityRun(t, f.st, authz.ModeEnforce, "dave")
	// support-tier-1 denies dave agent:admin/**: he may not ask who can.
	_, res := callIdentity(t, i, dave, `{"op":"can","subject":"erin","action":"run","resource":{"type":"agent","id":"admin/triage"}}`)
	wantNotFound(t, res, "a member asking who may do what they may not")
	_, res = callIdentity(t, i, dave, `{"op":"explain","subject":"erin","action":"run","resource":{"type":"agent","id":"admin/triage"}}`)
	wantNotFound(t, res, "explain of the same question")
	_, res = callIdentity(t, i, dave, `{"op":"can","subject":"erin","queries":[{"action":"run","resource":{"type":"agent","id":"chat/a"}},{"action":"run","resource":{"type":"agent","id":"admin/triage"}}]}`)
	wantNotFound(t, res, "a batch with one stronger question")
	// A question dave may answer yes to himself is answered for erin.
	out, res := callIdentity(t, i, dave, `{"op":"can","subject":"erin","action":"run","resource":{"type":"agent","id":"chat/a"}}`)
	if res.IsError || out["subject"] != "erin" || out["outcome"] == nil {
		t.Errorf("a question within the caller's own reach: %s", res.Text)
	}
	// compat: a member may not ask about anyone else at all.
	_, res = callIdentity(t, f.tool(authz.ModeCompat), identityRun(t, f.st, authz.ModeCompat, "dave"),
		`{"op":"can","subject":"erin","action":"run","resource":{"type":"agent","id":"chat/a"}}`)
	wantNotFound(t, res, "a compat member asking about another")
}

// can for another person answers what that person's own gate answers: the
// set compiled for their class, under the caller's mode and transport.
func TestIdentityCan_AnswersWithTheSubjectsOwnSet(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeEnforce)
	op := f.operatorRun(t, authz.ModeEnforce)
	p, err := aclres.New(f.st, authz.ModeEnforce, 0).Resolve(context.Background(), aclres.RowPrincipal("acme", "dave", "tenant"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct{ action, typ, id string }{
		{"run", "agent", "admin/triage"}, {"run", "agent", "support/x"}, {"invoke", "tool", "Read"}, {"write", "memory", "tenant"},
	} {
		out, res := callIdentity(t, i, op, fmt.Sprintf(`{"op":"can","subject":"dave","action":%q,"resource":{"type":%q,"id":%q}}`, q.action, q.typ, q.id))
		if res.IsError {
			t.Fatal(res.Text)
		}
		want := canAnswer(op, p.Effective, q.action, authz.Resource{Type: q.typ, Name: q.id})
		for _, k := range []string{"decision", "outcome", "allowed"} {
			if fmt.Sprint(out[k]) != fmt.Sprint(want[k]) {
				t.Errorf("%v: %s = %v, the subject's own set says %v", q, k, out[k], want[k])
			}
		}
	}
	// no-shell denies Bash to the whole tenant, the operator included: the
	// operator may not ask who may use it either.
	_, res := callIdentity(t, i, op, `{"op":"can","subject":"dave","action":"invoke","resource":{"type":"tool","id":"Bash"}}`)
	wantNotFound(t, res, "an operator asking about a tool denied to the operator")
	// alice is pending: whatever her rules say, she is refused everything.
	out, _ := callIdentity(t, i, op, `{"op":"can","subject":"alice","action":"run","resource":{"type":"agent","id":"chat/a"}}`)
	if out["allowed"] != false || out["account_status"] != "pending" {
		t.Errorf("a pending account: %v", out)
	}
}

func TestIdentityExplain_RedactsListsTheCallerMayNotRead(t *testing.T) {
	f := newIdentityFixture(t)
	f.grant(t, "directory-read", "dave", readPrincipalsRules)
	q := `{"op":"explain","action":"run","resource":{"type":"agent","id":"chat/a"}}`
	out, res := callIdentity(t, f.tool(authz.ModeEnforce), identityRun(t, f.st, authz.ModeEnforce, "dave"), q)
	if res.IsError {
		t.Fatal(res.Text)
	}
	traces := out["traces"].([]any)
	if len(traces) == 0 {
		t.Fatal("no traces")
	}
	for _, tr := range traces {
		keys := []string{}
		for k := range tr.(map[string]any) {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"acl", "effect", "rule"}) {
			t.Errorf("a member sees a trace of a list it may not read with %v", keys)
		}
	}
	// The operator may read every list: full traces.
	out, _ = callIdentity(t, f.tool(authz.ModeEnforce), f.operatorRun(t, authz.ModeEnforce), `{"op":"explain","subject":"dave","action":"run","resource":{"type":"agent","id":"chat/a"}}`)
	for _, tr := range out["traces"].([]any) {
		if _, ok := tr.(map[string]any)["matched"]; !ok {
			t.Errorf("an operator's trace is redacted: %v", tr)
		}
	}
}

func TestIdentityGroup_AMemberSeesTheirOwnGroupWithoutItsMembers(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeCompat)
	dave := identityRun(t, f.st, authz.ModeCompat, "dave")
	out, res := callIdentity(t, i, dave, `{"op":"group","name":"support"}`)
	if res.IsError || out["members_withheld"] != true || out["members"] != nil || out["description"] != "Tier 1" ||
		!reflect.DeepEqual(out["contained_in"], []any{"staff"}) {
		t.Errorf("a member's own group: %s", res.Text)
	}
	_, res = callIdentity(t, i, dave, `{"op":"group","name":"eng"}`)
	wantNotFound(t, res, "a group the member is not in")
	_, res = callIdentity(t, i, dave, `{"op":"group","name":"nosuch"}`)
	wantNotFound(t, res, "a missing group")
	out, _ = callIdentity(t, i, dave, `{"op":"groups"}`)
	var names []string
	for _, g := range out["groups"].([]any) {
		names = append(names, g.(map[string]any)["name"].(string))
	}
	if !reflect.DeepEqual(names, []string{"staff", "support"}) {
		t.Errorf("a member lists groups %v, want only their own", names)
	}
	_, res = callIdentity(t, i, dave, `{"op":"users","group":"support"}`)
	wantNotFound(t, res, "a member listing the members of their group")

	op := f.operatorRun(t, authz.ModeCompat)
	out, res = callIdentity(t, i, op, `{"op":"group","name":"support"}`)
	if res.IsError || out["members_withheld"] != nil || len(out["members"].([]any)) != 2 {
		t.Errorf("an operator's group: %s", res.Text)
	}
	out, _ = callIdentity(t, i, op, `{"op":"users","group":"staff"}`)
	var subjects []string
	for _, u := range out["users"].([]any) {
		subjects = append(subjects, u.(map[string]any)["subject"].(string))
	}
	if !reflect.DeepEqual(subjects, []string{"alice", "dave"}) {
		t.Errorf("users in staff (through support) = %v", subjects)
	}
}

func TestIdentityHeld_NeedsTheManageGrant(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeCompat)
	_, res := callIdentity(t, i, identityRun(t, f.st, authz.ModeCompat, "dave"), `{"op":"held","subject":"erin"}`)
	wantNotFound(t, res, "a member asking what is held for another")
	out, res := callIdentity(t, i, f.operatorRun(t, authz.ModeCompat), `{"op":"held","subject":"erin"}`)
	if res.IsError || out["subject"] != "erin" || out["tenant"] != "acme" {
		t.Errorf("an operator's held: %s", res.Text)
	}
	// enforce: a list may withhold manage from an operator.
	f.grant(t, "no-manage", "op1", `[{"effect":"deny","actions":["manage"],"resources":["principal:user:*"]}]`)
	_, res = callIdentity(t, f.tool(authz.ModeEnforce), f.operatorRun(t, authz.ModeEnforce), `{"op":"held","subject":"erin"}`)
	wantNotFound(t, res, "an operator denied manage")
}

// A sub-agent reads the directory as the person who started the tree: the
// set on the run policy decides, whatever principal the ctx carries.
func TestIdentity_ASubAgentReadsWithTheOriginatingUsersSet(t *testing.T) {
	f := newIdentityFixture(t)
	member := identityRun(t, f.st, authz.ModeCompat, "dave")
	child := auth.WithPrincipal(member, auth.Principal{TenantID: "acme", Subject: "op1", Scopes: []string{auth.ScopeTenant}})
	_, res := callIdentity(t, f.tool(authz.ModeCompat), child, `{"op":"user","subject":"erin"}`)
	wantNotFound(t, res, "a child of a member's run")
}

func TestIdentity_ADenyOfTheToolWithholdsIt(t *testing.T) {
	f := newIdentityFixture(t)
	f.grant(t, "no-directory", "dave", `[{"effect":"deny","actions":["invoke"],"resources":["tool:Identity"]}]`)
	ctx := identityRun(t, f.st, authz.ModeCompat, "dave")
	kept, removed := tools.NarrowToolsByACL(ctx, tools.CallerACL(ctx), []tools.Tool{&Identity{}, &Context{}})
	if len(kept) != 1 || kept[0].Name() != "Context" || !reflect.DeepEqual(removed, []string{"Identity"}) {
		t.Errorf("kept %d, removed %v", len(kept), removed)
	}
}

// The op enum is the whole surface, and nothing in it writes.
func TestIdentity_HasNoMutatingOp(t *testing.T) {
	ops := tools.SchemaEnum((&Identity{}).InputSchema(), "op")
	want := []string{"profile", "user", "users", "group", "groups", "can", "explain", "held", "whoami"}
	if !reflect.DeepEqual(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
}

// No answer carries a credential, a token suffix of anyone, an e-mail or a
// hash: a census over every key every op returns, for an operator who may
// see the most.
func TestIdentity_NoOutputKeyNamesSecretMaterial(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeCompat)
	ctx := f.operatorRun(t, authz.ModeCompat)
	keys := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				keys[k] = true
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, in := range []string{
		`{"op":"profile","subject":"dave"}`, `{"op":"user","subject":"dave"}`, `{"op":"users"}`,
		`{"op":"users","active_only":true}`, `{"op":"group","name":"support"}`, `{"op":"groups"}`,
		`{"op":"can","subject":"dave","action":"run","resource":{"type":"agent","id":"chat/a"}}`,
		`{"op":"explain","subject":"dave","action":"run","resource":{"type":"agent","id":"chat/a"}}`,
		`{"op":"held","subject":"dave"}`, `{"op":"whoami"}`,
	} {
		out, res := callIdentity(t, i, ctx, in)
		if res.IsError {
			t.Fatalf("%s: %s", in, res.Text)
		}
		walk(out)
	}
	secret := regexp.MustCompile(`(?i)token|suffix|hash|secret|password|bearer|credential|email|mail`)
	for k := range keys {
		if secret.MatchString(k) {
			t.Errorf("an Identity answer carries the key %q", k)
		}
	}
}

func TestIdentity_PagesAndBatchesAreBounded(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	for n := 0; n < 198; n++ { // + alice, dave, erin = 201
		if err := f.st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: fmt.Sprintf("u%03d", n), AccessMode: "tenant", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	i := f.tool(authz.ModeCompat)
	op := f.operatorRun(t, authz.ModeCompat)
	out, res := callIdentity(t, i, op, `{"op":"users","limit":200}`)
	if res.IsError || len(out["users"].([]any)) != 200 || out["next_cursor"] == nil {
		t.Fatalf("first page: %d rows, cursor %v", len(out["users"].([]any)), out["next_cursor"])
	}
	seen := map[string]bool{}
	for _, u := range out["users"].([]any) {
		seen[u.(map[string]any)["subject"].(string)] = true
	}
	out, res = callIdentity(t, i, op, `{"op":"users","limit":200,"cursor":"`+out["next_cursor"].(string)+`"}`)
	if res.IsError || len(out["users"].([]any)) != 1 || out["next_cursor"] != nil {
		t.Fatalf("second page: %s", res.Text)
	}
	if last := out["users"].([]any)[0].(map[string]any)["subject"].(string); seen[last] {
		t.Errorf("the second page repeats %s", last)
	}
	if _, res := callIdentity(t, i, op, `{"op":"users","limit":201}`); !res.IsError {
		t.Error("a limit over 200 was accepted")
	}
	qs := make([]string, 33)
	for k := range qs {
		qs[k] = `{"action":"run","resource":{"type":"agent","id":"chat/a"}}`
	}
	if _, res := callIdentity(t, i, op, `{"op":"can","subject":"dave","queries":[`+strings.Join(qs, ",")+`]}`); !res.IsError || !strings.Contains(res.Text, "limit is 32") {
		t.Errorf("33 questions: %s", res.Text)
	}
}

// Every lookup of another person or a group is audited with what came of it
// — the record tells a refusal from a miss, which the caller is not told.
// Looking yourself up is not audited.
func TestIdentity_AuditsLookupsOfOthersOnly(t *testing.T) {
	f := newIdentityFixture(t)
	i := f.tool(authz.ModeCompat)
	dave := identityRun(t, f.st, authz.ModeCompat, "dave")
	for _, in := range []string{
		`{"op":"profile","subject":"dave"}`, `{"op":"user","subject":"dave"}`, `{"op":"whoami"}`,
		`{"op":"profile","subject":"erin"}`, `{"op":"user","subject":"erin"}`, `{"op":"user","subject":"nobody"}`,
		`{"op":"group","name":"eng"}`,
	} {
		callIdentity(t, i, dave, in)
	}
	var got []string
	for _, ev := range f.sink.evs {
		if ev.Action != "identity_lookup" || ev.ActorSubject != "dave" || ev.TargetTenant != "acme" || ev.IdentityRunID != "r_identity" {
			t.Errorf("event %+v", ev)
		}
		got = append(got, ev.IdentityOp+":"+ev.TargetSubject+ev.TargetName+":"+ev.IdentityOutcome)
	}
	want := []string{"profile:erin:ok", "user:erin:refused", "user:nobody:not_found", "group:eng:refused"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestIdentityWhoami_IsContextsIdentityBlock(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := identityRun(t, f.st, authz.ModeCompat, "alice")
	who, res := callIdentity(t, f.tool(authz.ModeCompat), ctx, `{"op":"whoami"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	self, _ := callContext(t, &Context{Store: f.st}, ctx, `{"op":"self"}`)
	if !reflect.DeepEqual(who, self["identity"]) {
		t.Errorf("whoami = %v\nContext op=self identity = %v", who, self["identity"])
	}
}

func TestIdentity_RefusesMalformedNamesBeforeAnyRead(t *testing.T) {
	f := newIdentityFixture(t)
	rec := &recordingStore{Store: f.st}
	i := &Identity{Store: rec, SqlMem: f.mgr, ACL: aclres.New(f.st, authz.ModeCompat, 0)}
	ctx := f.operatorRun(t, authz.ModeCompat)
	for _, in := range []string{
		`{"op":"profile","subject":""}`, `{"op":"user","subject":" alice"}`, `{"op":"user","subject":"a\u0000b"}`,
		`{"op":"held","subject":"` + strings.Repeat("x", 300) + `"}`, `{"op":"group","name":"../x"}`, `{"op":"users","group":"a b"}`,
	} {
		if _, res := callIdentity(t, i, ctx, in); !res.IsError {
			t.Errorf("%s accepted", in)
		}
	}
	if len(rec.users) != 0 || len(rec.group) != 0 {
		t.Errorf("a malformed name reached the store: %v %v", rec.users, rec.group)
	}
}
