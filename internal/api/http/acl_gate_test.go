package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// aclFixture is the restricted-token fixture with the RFC EY resolver wired
// in the given mode and a ttl of zero (the tenant version is re-read on
// every request, so a store write is visible to the next call).
func aclFixture(t *testing.T, mode authz.Mode) (*Server, string, store.Store) {
	t.Helper()
	srv, ts, st := restrictedFixtureServer(t)
	srv.SetACLResolver(aclres.New(st, mode, 0))
	return srv, ts.URL, st
}

func putACL(t *testing.T, st store.Store, name, rules string) {
	t.Helper()
	if _, err := st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: name, Rules: json.RawMessage(rules)}); err != nil {
		t.Fatal(err)
	}
}

func bind(t *testing.T, st store.Store, acl, kind, id string) string {
	t.Helper()
	b, err := st.AclBindingPut(context.Background(), store.AclBindingRow{TenantID: "acme", ACLName: acl, SubjectKind: kind, SubjectID: id})
	if err != nil {
		t.Fatal(err)
	}
	return b.BindingID
}

// The owner's first case end to end: a deny on an agent mask, bound to a
// GROUP the token's subject is in through a nested group, refuses the run
// with the rule named; the agents the mask does not cover still start; a
// subject outside the group is untouched; and retiring the binding lifts the
// refusal on the next request.
func TestACLGate_GroupDenyOnAgentMaskRefusesTheRun(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeCompat)
	ctx := context.Background()
	putACL(t, st, "no-other", `[{"effect":"deny","actions":["run"],"resources":["agent:oth*"]}]`)
	for _, g := range []string{"staff", "support"} {
		if _, err := st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "staff", MemberKind: "group", MemberID: "support"}); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "user", MemberID: "dev"}); err != nil {
		t.Fatal(err)
	}
	id := bind(t, st, "no-other", "group", "staff")

	code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("other"))
	if code != 403 || !strings.Contains(body, `"code":"acl_denied"`) || !strings.Contains(body, `"acl":"no-other"`) {
		t.Fatalf("denied agent: status %d: %s", code, body)
	}
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("allowed")); code != 200 {
		t.Fatalf("an agent the mask does not cover: status %d: %s", code, body)
	}
	// Whoami shows the closed groups and the mode.
	code, body = bearerCall(t, httptestServerAt(url), freeBearer, "GET", "/v1/_me", "")
	if code != 200 || !strings.Contains(body, `"groups":["staff","support"]`) || !strings.Contains(body, `"acl_mode":"compat"`) {
		t.Errorf("whoami: %d %s", code, body)
	}
	// Retire the binding: the next request is admitted (ttl 0, version re-read).
	if ok, err := st.AclBindingRetire(ctx, "acme", id); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("other")); code != 200 {
		t.Errorf("after retiring the binding: status %d: %s", code, body)
	}
}

// A type-level deny on a route family holds at the middleware, before any
// handler runs, and a deny on SOME instances does not (the instance check
// decides those).
func TestACLGate_TypeLevelDenyHoldsAtTheRoute(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeCompat)
	putACL(t, st, "no-reads", `[{"effect":"deny","actions":["read"],"resources":["run:*"]},{"effect":"deny","actions":["run"],"resources":["agent:admin/*"]}]`)
	bind(t, st, "no-reads", "user", "dev")
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "GET", "/v1/runs", ""); code != 403 || !strings.Contains(body, "acl_denied") {
		t.Errorf("GET /v1/runs under deny read run:*: %d %s", code, body)
	}
	// The partial agent deny does not refuse the route; the run of an
	// uncovered agent goes through.
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("allowed")); code != 200 {
		t.Errorf("POST /v1/runs with a partial deny: %d %s", code, body)
	}
	// Whoami is ungoverned and still answers.
	if code, _ := bearerCall(t, httptestServerAt(url), freeBearer, "GET", "/v1/_me", ""); code != 200 {
		t.Errorf("whoami: %d", code)
	}
}

// Enforce mode: the baseline admits what the member's class allows, an
// unmatched question is refused, and a tenant operator keeps its reach.
func TestACLGate_EnforceModeDecides(t *testing.T) {
	_, url, _ := aclFixture(t, authz.ModeEnforce)
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("allowed")); code != 200 {
		t.Errorf("enforce: member baseline should admit a run: %d %s", code, body)
	}
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "GET", "/v1/users/dev/agents", ""); code != 200 {
		t.Errorf("enforce: member baseline should admit reading runs: %d %s", code, body)
	}
	// The member baseline grants no operator control: a route the scope gate
	// would also refuse stays refused, now by the engine (the scope gate runs
	// first, so the code is still the scope one).
	if code, _ := bearerCall(t, httptestServerAt(url), freeBearer, "PUT", "/v1/_limits", `{}`); code != 403 {
		t.Errorf("enforce: member on PUT /v1/_limits: %d", code)
	}
	// The legacy admin keeps everything under the admin baseline.
	if code, body := bearerCall(t, httptestServerAt(url), "legacy-admin-secret", "GET", "/v1/_me", ""); code != 200 || !strings.Contains(body, `"acl_mode":"enforce"`) {
		t.Errorf("legacy admin whoami: %d %s", code, body)
	}
}

// Shadow mode never refuses: the legacy gate decides and the divergence is
// recorded.
func TestACLGate_ShadowModeNeverRefuses(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeShadow)
	putACL(t, st, "no-other", `[{"effect":"deny","actions":["run"],"resources":["agent:other"]}]`)
	bind(t, st, "no-other", "user", "dev")
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("other")); code != 200 {
		t.Errorf("shadow: status %d: %s", code, body)
	}
}

// The RFC EF allowlist and the ACL compose: the restricted token is still
// refused its unlisted agent with the EF code, and its listed agent can be
// refused by an ACL.
func TestACLGate_ComposesWithTheTokenAllowlist(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeCompat)
	if code, body := bearerCall(t, httptestServerAt(url), restrictedBearer, "POST", "/v1/runs", runBody("other")); code != 403 || !strings.Contains(body, "agent_not_allowed") {
		t.Errorf("EF allowlist first: %d %s", code, body)
	}
	putACL(t, st, "no-allowed", `[{"effect":"deny","actions":["run"],"resources":["agent:allowed"]}]`)
	bind(t, st, "no-allowed", "token", "def_restricted")
	if code, body := bearerCall(t, httptestServerAt(url), restrictedBearer, "POST", "/v1/runs", runBody("allowed")); code != 403 || !strings.Contains(body, "acl_denied") {
		t.Errorf("ACL on the listed agent, bound to the token: %d %s", code, body)
	}
	// The binding is to the token definition: the same subject's other
	// token is untouched.
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("allowed")); code != 200 {
		t.Errorf("another token of the subject: %d %s", code, body)
	}
}
