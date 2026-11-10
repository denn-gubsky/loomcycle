package builtin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// tenantOpCtx is a tenant operator of tenant "tnt" calling the tool, with the
// grant the transports derive from its principal.
func tenantOpCtx(t *testing.T, base context.Context, p auth.Principal) context.Context {
	t.Helper()
	pol := tools.OperatorTokenDefPolicyFor(p, true)
	if !pol.TenantScrape || pol.Admin {
		t.Fatalf("the fixture is wrong: %+v gets %+v", p, pol)
	}
	return tools.WithOperatorTokenDefPolicy(auth.WithPrincipal(base, p), pol)
}

var tntOp = auth.Principal{TenantID: "tnt", Subject: "u1", Scopes: []string{auth.ScopeTenant}}

func execRaw(t *testing.T, tool *OperatorTokenDef, ctx context.Context, in string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatalf("Execute(%s): %v", in, err)
	}
	return res
}

func storedRow(t *testing.T, st *sqlite.Store, out map[string]any) store.OperatorTokenDefRow {
	t.Helper()
	row, err := st.OperatorTokenDefGet(context.Background(), out["def_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func countTokenRows(t *testing.T, st *sqlite.Store) int {
	t.Helper()
	names, err := st.OperatorTokenDefListNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range names {
		n += s.TokenCount
	}
	return n
}

// A tenant operator mints a scrape token in its own tenant: metrics:read,
// tenant_metrics always on, named inside the per-user namespace.
func TestOperatorTokenDef_ATenantOperatorMintsItsOwnTenantsScrapeToken(t *testing.T) {
	tool, base, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ctx := tenantOpCtx(t, base, tntOp)
	for _, in := range []string{
		`{"op":"create","name":"prom","scopes":["metrics:read"]}`,
		`{"op":"create","name":"prom2","scopes":["metrics:read"],"tenant_id":"tnt","tenant_metrics":true}`,
	} {
		out := mustOp(t, tool, ctx, in)
		if out["token"] == "" {
			t.Errorf("%s: no token shown", in)
		}
		row := storedRow(t, st, out)
		r, err := auth.ParseRestrictions(row.RestrictionsJSON)
		if err != nil || !r.TenantMetrics {
			t.Errorf("%s: restrictions %q (%v), want tenant_metrics", in, row.RestrictionsJSON, err)
		}
		if row.TenantID != "tnt" || len(row.AllowedScopes) != 1 || row.AllowedScopes[0] != auth.ScopeMetricsRead {
			t.Errorf("%s: row tenant=%q scopes=%v", in, row.TenantID, row.AllowedScopes)
		}
		if !strings.HasPrefix(row.Name, scrapeTokenNamePrefix) || !strings.HasPrefix(row.Subject, "metrics-") {
			t.Errorf("%s: name=%q subject=%q", in, row.Name, row.Subject)
		}
	}
}

// Everything but a confined scrape token in its own tenant is refused, and
// nothing is stored.
func TestOperatorTokenDef_ATenantOperatorIsRefusedEverythingButItsScrapeToken(t *testing.T) {
	tool, base, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ctx := tenantOpCtx(t, base, tntOp)
	for _, in := range []string{
		`{"op":"create","name":"x"}`,
		`{"op":"create","name":"x","scopes":["substrate:tenant"]}`,
		`{"op":"create","name":"x","scopes":["substrate:admin"]}`,
		`{"op":"create","name":"x","scopes":["runs:create"]}`,
		`{"op":"create","name":"x","scopes":["metrics:read","runs:read"]}`,
		`{"op":"create","name":"x","scopes":["auth:manage"]}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"tenant_id":"other"}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"tenant_metrics":false}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"import_token":"0123456789abcdef"}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"agents":["chat"]}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"retention":"none"}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"rate":{"per_minute":5}}`,
		`{"op":"create","name":"bad name!","scopes":["metrics:read"]}`,
		`{"op":"purge"}`,
	} {
		if res := execRaw(t, tool, ctx, in); !res.IsError {
			t.Errorf("%s: accepted: %s", in, res.Text)
		}
	}
	if n := countTokenRows(t, st); n != 0 {
		t.Errorf("%d rows stored by refused creates", n)
	}
}

// Every token that is not one of its tenant's confined scrape tokens is,
// to a tenant operator, a token that does not exist: get, rotate and retire
// answer as for a missing id, and list leaves it out.
func TestOperatorTokenDef_ATenantOperatorSeesOnlyItsTenantsScrapeTokens(t *testing.T) {
	tool, admin, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ctx := tenantOpCtx(t, admin, tntOp)
	hidden := map[string]string{}
	for label, in := range map[string]string{
		"admin token":             `{"op":"create","name":"root","tenant_id":"tnt","scopes":["substrate:admin"]}`,
		"tenant token":            `{"op":"create","name":"ops","tenant_id":"tnt","scopes":["substrate:tenant"]}`,
		"unconfined scrape token": `{"op":"create","name":"fullprom","tenant_id":"tnt","scopes":["metrics:read"]}`,
		"other tenant's scrape":   `{"op":"create","name":"otherprom","tenant_id":"other","scopes":["metrics:read"],"tenant_metrics":true}`,
	} {
		hidden[label] = mustOp(t, tool, admin, in)["def_id"].(string)
	}
	// The other tenant's operator holds the name this one is about to take.
	other := tenantOpCtx(t, admin, auth.Principal{TenantID: "other", Subject: "o", Scopes: []string{auth.ScopeTenant}})
	hidden["other tenant's same name"] = mustOp(t, tool, other, `{"op":"create","name":"prom","scopes":["metrics:read"]}`)["def_id"].(string)
	// An admin may mint a confined scrape token in the tenant; it is the
	// tenant's to manage. And the other tenant's name does not block this one.
	adminMinted := mustOp(t, tool, admin, `{"op":"create","name":"tntprom","tenant_id":"tnt","scopes":["metrics:read"],"tenant_metrics":true}`)["def_id"].(string)
	own := mustOp(t, tool, ctx, `{"op":"create","name":"prom","scopes":["metrics:read"]}`)["def_id"].(string)

	for label, id := range hidden {
		for _, op := range []string{"get", "rotate", "retire"} {
			res := execRaw(t, tool, ctx, `{"op":"`+op+`","def_id":"`+id+`"}`)
			missing := execRaw(t, tool, ctx, `{"op":"`+op+`","def_id":"tok_does_not_exist"}`)
			want := strings.ReplaceAll(missing.Text, "tok_does_not_exist", id)
			if !res.IsError || res.Text != want {
				t.Errorf("%s %s: %s, want the missing-id answer %s", op, label, res.Text, want)
			}
		}
		if row, err := st.OperatorTokenDefGet(context.Background(), id); err != nil || !row.RetiredAt.IsZero() {
			t.Errorf("%s: retired or gone after the refusals (%v)", label, err)
		}
	}
	res := execRaw(t, tool, ctx, `{"op":"list"}`)
	var list struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(res.Text), &list); err != nil {
		t.Fatalf("list %s: %v", res.Text, err)
	}
	got := map[string]bool{}
	for _, tok := range list.Tokens {
		got[tok["def_id"].(string)] = true
	}
	if len(got) != 2 || !got[own] || !got[adminMinted] {
		t.Errorf("list = %s, want exactly %s and %s", res.Text, own, adminMinted)
	}
	if out := mustOp(t, tool, ctx, `{"op":"get","def_id":"`+adminMinted+`"}`); out["def_id"] != adminMinted {
		t.Errorf("get of an admin-minted confined token in the tenant: %v", out)
	}
}

// Rotation keeps the tenant and the marker, and retire ends the token.
func TestOperatorTokenDef_ATenantOperatorRotatesAndRetiresItsScrapeToken(t *testing.T) {
	tool, base, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ctx := tenantOpCtx(t, base, tntOp)
	first := mustOp(t, tool, ctx, `{"op":"create","name":"prom","scopes":["metrics:read"]}`)
	rotated := mustOp(t, tool, ctx, `{"op":"rotate","name":"`+first["name"].(string)+`","grace_seconds":0}`)
	row := storedRow(t, st, rotated)
	if r, err := auth.ParseRestrictions(row.RestrictionsJSON); err != nil || !r.TenantMetrics || row.TenantID != "tnt" {
		t.Errorf("rotated row tenant=%q restrictions=%q (%v)", row.TenantID, row.RestrictionsJSON, err)
	}
	mustOp(t, tool, ctx, `{"op":"retire","def_id":"`+rotated["def_id"].(string)+`"}`)
	if row := storedRow(t, st, rotated); row.RetiredAt.IsZero() {
		t.Error("retire left the token live")
	}
}

// A scrape token cannot outlive the tenant operator's token that minted it.
func TestOperatorTokenDef_ATenantScrapeTokenIsBoundByItsMinter(t *testing.T) {
	tool, base, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ends := time.Now().Add(time.Hour).UTC()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_op", Name: "op", TenantID: "tnt", Subject: "u1", TokenHash: "h", AllowedScopes: []string{auth.ScopeTenant}, ExpiresAt: ends,
	}); err != nil {
		t.Fatal(err)
	}
	p := tntOp
	p.TokenDefID = "def_op"
	ctx := tenantOpCtx(t, base, p)
	if res := execRaw(t, tool, ctx, `{"op":"create","name":"long","scopes":["metrics:read"],"ttl_seconds":7200}`); !res.IsError || !strings.Contains(res.Text, "outlive") {
		t.Errorf("a lifetime past the minter's: %s, want refused", res.Text)
	}
	row := storedRow(t, st, mustOp(t, tool, ctx, `{"op":"create","name":"bounded","scopes":["metrics:read"]}`))
	if !row.ExpiresAt.Equal(ends) && row.ExpiresAt.Sub(ends).Abs() > time.Millisecond {
		t.Errorf("unasked lifetime ends %v, want the minter's %v", row.ExpiresAt, ends)
	}
}

// An admin may confine a scrape token; the marker fits nothing else.
func TestOperatorTokenDef_TenantMetricsOnlyOnAMetricsReadToken(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	row := storedRow(t, st, mustOp(t, tool, ctx, `{"op":"create","name":"p","tenant_id":"ops","scopes":["metrics:read"],"tenant_metrics":true}`))
	if r, err := auth.ParseRestrictions(row.RestrictionsJSON); err != nil || !r.TenantMetrics {
		t.Errorf("admin-minted confined scrape token: %q (%v)", row.RestrictionsJSON, err)
	}
	for _, in := range []string{
		`{"op":"create","name":"a","tenant_id":"ops","scopes":["substrate:tenant"],"tenant_metrics":true}`,
		`{"op":"create","name":"b","tenant_id":"ops","scopes":["runs:create","runs:read"],"tenant_metrics":true}`,
	} {
		if res := execRaw(t, tool, ctx, in); !res.IsError || !strings.Contains(res.Text, "tenant_metrics") {
			t.Errorf("%s: %s, want refused", in, res.Text)
		}
	}
}

// A tenant operator's token change is recorded with the operator as actor.
func TestOperatorTokenDef_ATenantOperatorsChangeIsAuditedWithItAsActor(t *testing.T) {
	tool, base, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	sink := &recordingSink{}
	tool.Audit = sink
	out := mustOp(t, tool, tenantOpCtx(t, base, tntOp), `{"op":"create","name":"prom","scopes":["metrics:read"]}`)
	if len(sink.evs) == 0 {
		t.Fatal("no audit record")
	}
	ev := sink.evs[0]
	if ev.Action != "create" || ev.ActorTenant != "tnt" || ev.ActorSubject != "u1" || ev.TargetTenant != "tnt" || ev.TargetDefID != out["def_id"] {
		t.Errorf("audit = %+v", ev)
	}
}

// The per-op question: create and rotate ask mint, retire manage, get and
// list read — of principal:token:metrics for a tenant operator and of
// principal:token:* for an admin — so a rule on the right verb refuses
// exactly that op.
func TestOperatorTokenDefACL_AsksTheOpsVerbOfTheTokenKind(t *testing.T) {
	tool, base, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	enforce := authz.WithMode(base, authz.ModeEnforce)
	seedCtx := tenantOpCtx(t, base, tntOp)
	seed := mustOp(t, tool, seedCtx, `{"op":"create","name":"seed","scopes":["metrics:read"]}`)["def_id"].(string)

	allowScrape := authz.Rule{Effect: authz.Allow, Actions: []string{"mint", "manage", "read"}, Resources: []string{"principal:token:metrics"}}
	withRules := func(rules ...authz.Rule) context.Context {
		p := tntOp
		p.Effective = aclSet(t, append([]authz.Rule{allowScrape}, rules...)...)
		return tenantOpCtx(t, enforce, p)
	}
	deny := func(verb string) authz.Rule {
		return authz.Rule{Effect: authz.Deny, Actions: []string{verb}, Resources: []string{"principal:token:metrics"}}
	}
	allowed(t, "create, allowed", execRaw(t, tool, withRules(), `{"op":"create","name":"ok","scopes":["metrics:read"]}`))
	deniedByACL(t, "create, mint denied", execRaw(t, tool, withRules(deny("mint")), `{"op":"create","name":"no","scopes":["metrics:read"]}`))
	deniedByACL(t, "rotate, mint denied", execRaw(t, tool, withRules(deny("mint")), `{"op":"rotate","def_id":"`+seed+`"}`))
	deniedByACL(t, "retire, manage denied", execRaw(t, tool, withRules(deny("manage")), `{"op":"retire","def_id":"`+seed+`"}`))
	deniedByACL(t, "get, read denied", execRaw(t, tool, withRules(deny("read")), `{"op":"get","def_id":"`+seed+`"}`))
	allowed(t, "get, mint denied", execRaw(t, tool, withRules(deny("mint")), `{"op":"get","def_id":"`+seed+`"}`))
	// A deny on every token covers the scrape tokens too.
	deniedByACL(t, "create, every token denied", execRaw(t, tool, withRules(authz.Rule{Effect: authz.Deny, Actions: []string{"mint"}, Resources: []string{"principal:token:*"}}),
		`{"op":"create","name":"no2","scopes":["metrics:read"]}`))
	// Enforce mode with no rule allowing the op refuses it.
	p := tntOp
	p.Effective = aclSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"principal:token:metrics"}})
	deniedByACL(t, "create, no rule", execRaw(t, tool, tenantOpCtx(t, enforce, p), `{"op":"create","name":"no3","scopes":["metrics:read"]}`))

	// The admin path asks of every token.
	admin := auth.Principal{TenantID: "tnt", Subject: "u1", Scopes: []string{auth.ScopeAdmin},
		Effective: aclSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"read", "mint", "manage"}, Resources: []string{"principal:*"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"mint"}, Resources: []string{"principal:token:*"}})}
	actx := tools.WithOperatorTokenDefPolicy(auth.WithPrincipal(enforce, admin), tools.OperatorTokenDefPolicyFor(admin, true))
	deniedByACL(t, "admin create, mint denied", execRaw(t, tool, actx, `{"op":"create","name":"adm","tenant_id":"tnt","scopes":["runs:read"]}`))
	allowed(t, "admin get, mint denied", execRaw(t, tool, actx, `{"op":"get","def_id":"`+seed+`"}`))
}

// deniedByACL asserts res is the tool's access-control refusal.
func deniedByACL(t *testing.T, what string, res tools.Result) {
	t.Helper()
	if code, ok := tools.ACLRefusalCode(res.Text, res.Error); !res.IsError || !ok || code != "acl_denied" {
		t.Errorf("%s: want acl_denied, got IsError=%v %s", what, res.IsError, res.Text)
	}
}

// A rotation hands the caller a fresh secret, so a tenant operator's
// rotation is held to its own token's end as its create is: the new token
// ends no later than the rotator, whatever the prior one carried.
func TestOperatorTokenDef_ATenantRotationIsBoundByTheRotatorsEnd(t *testing.T) {
	tool, admin, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ends := time.Now().Add(time.Hour).UTC()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_op", Name: "op", TenantID: "tnt", Subject: "u1", TokenHash: "h", AllowedScopes: []string{auth.ScopeTenant}, ExpiresAt: ends,
	}); err != nil {
		t.Fatal(err)
	}
	p := tntOp
	p.TokenDefID = "def_op"
	ctx := tenantOpCtx(t, admin, p)
	for name, in := range map[string]string{
		"no expiry":          `{"op":"create","name":"forever","tenant_id":"tnt","scopes":["metrics:read"],"tenant_metrics":true}`,
		"past the rotator":   `{"op":"create","name":"long","tenant_id":"tnt","scopes":["metrics:read"],"tenant_metrics":true,"ttl_seconds":86400}`,
		"before the rotator": `{"op":"create","name":"short","tenant_id":"tnt","scopes":["metrics:read"],"tenant_metrics":true,"ttl_seconds":600}`,
	} {
		prior := storedRow(t, st, mustOp(t, tool, admin, in))
		next := storedRow(t, st, mustOp(t, tool, ctx, `{"op":"rotate","def_id":"`+prior.DefID+`"}`))
		if next.ExpiresAt.IsZero() || next.ExpiresAt.After(ends) {
			t.Errorf("%s: rotated token ends %v, want no later than the rotator's %v", name, next.ExpiresAt, ends)
		}
		if !prior.ExpiresAt.IsZero() && next.ExpiresAt.After(prior.ExpiresAt) {
			t.Errorf("%s: rotated token ends %v, later than the prior %v", name, next.ExpiresAt, prior.ExpiresAt)
		}
	}
}

// A tenant operator may shorten the window in which a rotated-out secret
// still works, never stretch it past the server's default.
func TestOperatorTokenDef_ATenantRotationGraceIsCappedAtTheDefault(t *testing.T) {
	tool, base, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ctx := tenantOpCtx(t, base, tntOp)
	first := mustOp(t, tool, ctx, `{"op":"create","name":"prom","scopes":["metrics:read"]}`)
	over := int(tool.grace()/time.Second) + 1
	res := execRaw(t, tool, ctx, `{"op":"rotate","def_id":"`+first["def_id"].(string)+`","grace_seconds":`+strconv.Itoa(over)+`}`)
	if !res.IsError || !strings.Contains(res.Text, "grace_seconds") {
		t.Errorf("a grace past the default: %s, want refused", res.Text)
	}
	if row := storedRow(t, st, first); !row.RetiredAt.IsZero() {
		t.Error("the refused rotation retired the prior token")
	}
	mustOp(t, tool, ctx, `{"op":"rotate","def_id":"`+first["def_id"].(string)+`","grace_seconds":60}`)
}

// A retired token stays retired: a rotation of it is refused, on the admin
// path and a tenant operator's alike, and its retirement is not moved later,
// so its secret does not come back for a grace window.
func TestOperatorTokenDef_ARetiredTokenCannotBeRotatedBackToLife(t *testing.T) {
	tool, admin, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	tenant := tenantOpCtx(t, admin, tntOp)
	for name, c := range map[string]struct {
		ctx context.Context
		in  string
	}{
		"admin":           {admin, `{"op":"create","name":"svc","tenant_id":"tnt","scopes":["runs:read"]}`},
		"tenant operator": {tenant, `{"op":"create","name":"prom","scopes":["metrics:read"]}`},
	} {
		id := mustOp(t, tool, c.ctx, c.in)["def_id"].(string)
		mustOp(t, tool, c.ctx, `{"op":"retire","def_id":"`+id+`"}`)
		retired := storedRow(t, st, map[string]any{"def_id": id})
		for _, rot := range []string{`{"op":"rotate","def_id":"` + id + `"}`, `{"op":"rotate","def_id":"` + id + `","grace_seconds":3600}`} {
			if res := execRaw(t, tool, c.ctx, rot); !res.IsError {
				t.Errorf("%s: rotating a retired token: %s, want refused", name, res.Text)
			}
		}
		// Retiring it again does not move its end later either.
		execRaw(t, tool, c.ctx, `{"op":"retire","def_id":"`+id+`"}`)
		after := storedRow(t, st, map[string]any{"def_id": id})
		if after.RetiredAt.After(retired.RetiredAt) || after.ValidAt(time.Now()) {
			t.Errorf("%s: retired_at %v → %v (valid now: %v), want unchanged and invalid", name, retired.RetiredAt, after.RetiredAt, after.ValidAt(time.Now()))
		}
	}
	if n := countTokenRows(t, st); n != 2 {
		t.Errorf("%d rows, want the 2 created: a refused rotation minted a descendant", n)
	}
}

// Rotating a token still in its grace window does not stretch the window.
func TestOperatorTokenDef_ARotationNeverMovesAnEarlierRetirementLater(t *testing.T) {
	tool, admin, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	id := mustOp(t, tool, admin, `{"op":"create","name":"svc","tenant_id":"tnt","scopes":["runs:read"]}`)["def_id"].(string)
	mustOp(t, tool, admin, `{"op":"rotate","def_id":"`+id+`","grace_seconds":60}`)
	first := storedRow(t, st, map[string]any{"def_id": id}).RetiredAt
	mustOp(t, tool, admin, `{"op":"rotate","def_id":"`+id+`","grace_seconds":86400}`)
	if got := storedRow(t, st, map[string]any{"def_id": id}).RetiredAt; got.After(first) {
		t.Errorf("a second rotation moved retired_at from %v to %v", first, got)
	}
}
