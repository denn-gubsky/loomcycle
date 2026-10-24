package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const testPepper = "test-pepper-xyz"

func operatorTokenDefFixture(t *testing.T) (*OperatorTokenDef, context.Context, *sqlite.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	tool := &OperatorTokenDef{Store: s, Hasher: auth.NewTokenHasher(testPepper, "", ""), Audit: audit.NopSink{}}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_admin", UserID: "ops"})
	ctx = tools.WithOperatorTokenDefPolicy(ctx, tools.OperatorTokenDefPolicyValue{Admin: true})
	return tool, ctx, s, func() { _ = s.Close() }
}

func mustOp(t *testing.T, tool *OperatorTokenDef, ctx context.Context, in string) map[string]any {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatalf("Execute(%s): %v", in, err)
	}
	if res.IsError {
		t.Fatalf("Execute(%s) refused: %s", in, res.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode result %q: %v", res.Text, err)
	}
	return out
}

// Retiring the LAST admin-scoped token with no legacy fallback would flip the
// server into unauthenticated open mode (authConfigured → false) — a silent
// fail-open. The guard refuses it. With another admin token, or with a legacy
// fallback configured, the retire is allowed.
func TestOperatorTokenDef_RetireLastAdmin_RefusedWhenNoLegacyFallback(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t) // LegacyTokenSet=false
	defer cleanup()

	mustOp(t, tool, ctx, `{"op":"create","name":"ops","tenant_id":"acme","subject":"ops","scopes":["substrate:admin"]}`)

	// Retiring the only admin token → refused (would open the server).
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"retire","name":"ops"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "last admin-scoped token") {
		t.Fatalf("retiring the last admin token should be refused; got is_error=%v text=%q", res.IsError, res.Text)
	}

	// A second admin token exists → retiring the first is now allowed.
	mustOp(t, tool, ctx, `{"op":"create","name":"ops2","tenant_id":"acme","subject":"ops2","scopes":["substrate:admin"]}`)
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"retire","name":"ops"}`)); res.IsError {
		t.Fatalf("retiring one of two admin tokens should be allowed; got %s", res.Text)
	}
}

// An admin-scoped row that cannot authenticate under the configured pepper
// (here a derived-pepper form while an explicit pepper is set) is not an admin
// token the operator still has: it must not let the last usable one be
// retired.
func TestOperatorTokenDef_RetireLastAdmin_UnusableAdminRowDoesNotCount(t *testing.T) {
	tool, ctx, s, cleanup := operatorTokenDefFixture(t) // explicit pepper, LegacyTokenSet=false
	defer cleanup()
	mustOp(t, tool, ctx, `{"op":"create","name":"ops","tenant_id":"acme","subject":"ops","scopes":["substrate:admin"]}`)
	if _, err := s.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_tagged", Name: "tagged", TenantID: "acme", Subject: "tagged",
		TokenHash:     auth.NewTokenHasher("", "some-derived-pepper", "").Hash("lct_x"),
		AllowedScopes: []string{auth.ScopeAdmin},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"retire","name":"ops"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "last admin-scoped token") {
		t.Fatalf("retiring the last usable admin token was allowed: is_error=%v text=%q", res.IsError, res.Text)
	}
}

// A non-admin token is never the fail-open concern, so it can always be
// retired even as the only token; and with a legacy fallback the last-admin
// retire is permitted (it merely re-enables the legacy secret).
func TestOperatorTokenDef_RetireLastAdmin_AllowedWithLegacyOrNonAdmin(t *testing.T) {
	// Non-admin, no legacy: retire allowed.
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	mustOp(t, tool, ctx, `{"op":"create","name":"narrow","tenant_id":"acme","subject":"narrow","scopes":["runs:create"]}`)
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"retire","name":"narrow"}`)); res.IsError {
		t.Fatalf("retiring a non-admin token must always be allowed; got %s", res.Text)
	}

	// Last admin WITH a legacy fallback: allowed.
	tool2, ctx2, _, cleanup2 := operatorTokenDefFixture(t)
	defer cleanup2()
	tool2.LegacyTokenSet = func() bool { return true }
	mustOp(t, tool2, ctx2, `{"op":"create","name":"ops","tenant_id":"acme","subject":"ops","scopes":["substrate:admin"]}`)
	if res, _ := tool2.Execute(ctx2, json.RawMessage(`{"op":"retire","name":"ops"}`)); res.IsError {
		t.Fatalf("with a legacy fallback, retiring the last admin should be allowed; got %s", res.Text)
	}
}

// A config principal holding substrate:admin keeps auth on and keeps an admin
// credential (RFC ES F2), so the last admin token may be retired. A declared
// principal WITHOUT admin keeps auth on but would leave nobody able to manage
// tokens, so that retire is still refused.
func TestOperatorTokenDef_RetireLastAdmin_AllowedOnlyWithAnAdminPrincipal(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	tool.AdminPrincipalDeclared = func() bool { return true }
	mustOp(t, tool, ctx, `{"op":"create","name":"ops","tenant_id":"acme","subject":"ops","scopes":["substrate:admin"]}`)
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"retire","name":"ops"}`)); res.IsError {
		t.Fatalf("with an admin config principal, retiring the last admin token should be allowed; got %s", res.Text)
	}

	tool2, ctx2, _, cleanup2 := operatorTokenDefFixture(t) // no admin principal
	defer cleanup2()
	mustOp(t, tool2, ctx2, `{"op":"create","name":"ops","tenant_id":"acme","subject":"ops","scopes":["substrate:admin"]}`)
	if res, _ := tool2.Execute(ctx2, json.RawMessage(`{"op":"retire","name":"ops"}`)); !res.IsError {
		t.Fatal("with no admin config principal and no legacy secret, retiring the last admin token must stay refused")
	}
}

func TestOperatorTokenDef_CreateMintsTokenOnceAndStoresPepperedHash(t *testing.T) {
	tool, ctx, s, cleanup := operatorTokenDefFixture(t)
	defer cleanup()

	out := mustOp(t, tool, ctx, `{"op":"create","name":"alice","tenant_id":"acme","subject":"alice","scopes":["runs:create","runs:read"]}`)
	plaintext, _ := out["token"].(string)
	if !strings.HasPrefix(plaintext, "lct_") {
		t.Fatalf("token %q lacks lct_ prefix", plaintext)
	}
	if sfx, _ := out["token_suffix"].(string); len(sfx) != 6 {
		t.Errorf("token_suffix = %q, want 6 chars", sfx)
	}
	defID, _ := out["def_id"].(string)
	if defID == "" {
		t.Fatal("no def_id in create response")
	}

	// The stored hash must equal SHA-256(pepper‖plaintext) — and the
	// plaintext is NOT stored.
	row, err := s.OperatorTokenDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if row.TokenHash != auth.HashToken(testPepper, plaintext) {
		t.Errorf("stored hash does not match SHA-256(pepper‖token)")
	}
	if row.TokenHash == auth.HashToken("", plaintext) {
		t.Errorf("hash is NOT peppered (matches empty-pepper hash) — stolen-dump protection is absent")
	}
	if strings.Contains(row.TokenHash, plaintext) {
		t.Errorf("plaintext leaked into the stored hash")
	}

	// get must never re-expose the secret.
	got := mustOp(t, tool, ctx, `{"op":"get","def_id":"`+defID+`"}`)
	if _, ok := got["token"]; ok {
		t.Errorf("get response must not carry the token plaintext")
	}
	if _, ok := got["token_hash"]; ok {
		t.Errorf("get response must not carry the token hash")
	}
}

func TestOperatorTokenDef_RequiresAdmin(t *testing.T) {
	tool, _, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	// ctx WITHOUT the admin policy.
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_agent"})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "operator-admin") {
		t.Errorf("non-admin caller should be refused; got %s", res.Text)
	}
}

func TestOperatorTokenDef_CopyFromEnvImportsExistingSecret(t *testing.T) {
	tool, ctx, s, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	// Migration: bind the existing LOOMCYCLE_AUTH_TOKEN ("legacy-secret")
	// as an admin token instead of minting.
	out := mustOp(t, tool, ctx, `{"op":"create","name":"ops","tenant_id":"default","subject":"ops","import_token":"legacy-secret"}`)
	if out["imported"] != true {
		t.Errorf("response should mark imported=true; got %v", out["imported"])
	}
	if _, ok := out["token"]; ok {
		t.Error("import must NOT echo a plaintext token")
	}
	// The stored hash must be the hash of the imported secret, so the env
	// token now resolves to this principal.
	defID := out["def_id"].(string)
	row, err := s.OperatorTokenDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.TokenHash != auth.HashToken(testPepper, "legacy-secret") {
		t.Error("imported token_hash must equal SHA-256(pepper‖legacy-secret)")
	}
}

// ⚠️ THIS TEST USED TO ASSERT THE DEFECT: that an omitted scope list mints
// [substrate:admin]. It does not any more, because that made the failure mode
// of every mistake MAXIMUM PRIVILEGE — and self-locking, since one admin def
// disables the legacy LOOMCYCLE_AUTH_TOKEN login.
//
// Hit live on 2026-09-21: a caller sent `allowed_scopes` (what the RESPONSE and
// the stored column call the list), json.Unmarshal dropped the unknown key, and
// three tenant tokens came back as admin — two of them locking their deployment
// out of its own API.
func TestOperatorTokenDef_OmittedScopesIsRefusedNotEscalated(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"root","tenant_id":"default"}`))
	if !res.IsError {
		t.Fatalf("an omitted scope list minted a token instead of being refused: %s", res.Text)
	}
	// The error has to carry the fix, not just the fault: this is the message
	// an operator reads at the moment they cannot mint anything.
	for _, want := range []string{"scopes", auth.ScopeAdmin, "LOOMCYCLE_AUTH_TOKEN"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("refusal does not mention %q — an operator cannot act on it:\n%s", want, res.Text)
		}
	}
}

// THE REPORTED INCIDENT, in one call. `allowed_scopes` is not an unreasonable
// guess — it is what the response echoes and what the column is named — and it
// used to be dropped in silence and replaced with admin.
func TestOperatorTokenDef_TheWrongScopeKeyIsRefusedWithTheRightOne(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"create","name":"bench","tenant_id":"acme","allowed_scopes":["substrate:tenant"]}`))
	if !res.IsError {
		t.Fatalf("a mis-named scope key still minted a token: %s", res.Text)
	}
	if !strings.Contains(res.Text, "allowed_scopes") || !strings.Contains(res.Text, `"scopes"`) {
		t.Errorf("the refusal names neither the wrong key nor the right one:\n%s", res.Text)
	}
	// And nothing was created — a refused mint must not leave a def behind.
	if _, err := tool.Store.OperatorTokenDefGetCurrentByName(ctx, "bench"); err == nil {
		t.Error("a refused mint created a def anyway")
	}
}

// The one carve-out: binding the legacy LOOMCYCLE_AUTH_TOKEN means "this IS the
// admin token", and the migration cannot ask the operator to say so twice.
func TestOperatorTokenDef_ImportKeepsTheAdminDefault(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	out := mustOp(t, tool, ctx,
		`{"op":"create","name":"legacy","tenant_id":"default","import_token":"legacy-secret-value"}`)
	scopes, _ := out["allowed_scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != auth.ScopeAdmin {
		t.Errorf("import_token scopes = %v, want [%s] — the migration path still needs the default",
			scopes, auth.ScopeAdmin)
	}
}

func TestOperatorTokenDef_RejectsUnknownScope(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"x","tenant_id":"t","scopes":["runs:create","make-coffee"]}`))
	if !res.IsError || !strings.Contains(res.Text, "make-coffee") {
		t.Errorf("unknown scope should be refused naming it; got %s", res.Text)
	}
}

// auth:manage is a website's cross-tenant user-management credential; mixed
// into a wider token it would carry that reach into the other's surfaces, so
// it is minted alone (RFC FA D16).
func TestOperatorTokenDef_AuthManageIsMintedOnlyAlone(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"site-mixed","tenant_id":"t","scopes":["auth:manage","runs:create"]}`))
	if !res.IsError || !strings.Contains(res.Text, "only scope") {
		t.Fatalf("auth:manage + runs:create should be refused; got %s", res.Text)
	}
	mustOp(t, tool, ctx, `{"op":"create","name":"site","tenant_id":"t","scopes":["auth:manage"]}`)
}

// "Acme" and "acme" are two tenants to loomcycle but ONE volume directory on
// a case-insensitive filesystem. A mint that would create the second spelling
// is refused; the existing spelling keeps minting.
func TestOperatorTokenMint_RefusesCaseFoldCollisionWithExistingTenant(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	tool.ConfigTenants = []string{"globex"}
	mustOp(t, tool, ctx, `{"op":"create","name":"first","tenant_id":"acme","scopes":["runs:create"]}`)
	// A retired token's tenant still owns its volumes on disk.
	mustOp(t, tool, ctx, `{"op":"create","name":"old","tenant_id":"initech","scopes":["runs:create"]}`)
	mustOp(t, tool, ctx, `{"op":"retire","name":"old"}`)

	for _, tc := range []struct{ tenant, other string }{
		{"Acme", "acme"},       // an existing token's tenant
		{"INITECH", "initech"}, // a retired token's tenant
		{"GloBex", "globex"},   // a config principal's tenant
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"n-`+tc.tenant+`","tenant_id":"`+tc.tenant+`","scopes":["runs:create"]}`))
		if !res.IsError || !strings.Contains(res.Text, "differs only by case") || !strings.Contains(res.Text, tc.other) {
			t.Errorf("mint for tenant %q = %s, want a case-collision refusal naming %q", tc.tenant, res.Text, tc.other)
		}
	}
	// The exact spelling is the same tenant and keeps working.
	mustOp(t, tool, ctx, `{"op":"create","name":"second","tenant_id":"acme","scopes":["runs:create"]}`)
	mustOp(t, tool, ctx, `{"op":"create","name":"third","tenant_id":"globex","scopes":["runs:create"]}`)
}

// A tenant named after a reserved volume segment, in any case, would share the
// shared tenant's or the run-scoped volume tree.
func TestOperatorTokenMint_RefusesReservedTenantInAnyCase(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	for _, tenant := range []string{"_shared", "_Shared", "_EPHEMERAL"} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"r`+tenant+`","tenant_id":"`+tenant+`","scopes":["runs:create"]}`))
		if !res.IsError || !strings.Contains(res.Text, "reserved") {
			t.Errorf("mint for tenant %q = %s, want a reserved refusal", tenant, res.Text)
		}
	}
}

// The config side of the same rule, checked at boot against minted tokens: a
// principal added AFTER a token was minted for a case-twin tenant.
func TestCheckPrincipalTenantsAgainstTokens_RefusesCaseFoldCollision(t *testing.T) {
	tool, ctx, s, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	mustOp(t, tool, ctx, `{"op":"create","name":"first","tenant_id":"acme","scopes":["runs:create"]}`)

	err := CheckPrincipalTenantsAgainstTokens(ctx, s, []string{"", "Acme"})
	if err == nil || !strings.Contains(err.Error(), "differs only by case") {
		t.Errorf("err = %v, want a case-collision error", err)
	}
	if err := CheckPrincipalTenantsAgainstTokens(ctx, s, []string{"", "acme", "globex"}); err != nil {
		t.Errorf("identical spellings: err = %v, want nil", err)
	}
}

// A tenant whose only tokens are per-user ones, under a name another tenant
// minted later, is still a known tenant. The names listing merged rows by
// name and kept the newest row's tenant, so such a tenant was missing and its
// case-twin passed the check.
func TestCheckPrincipalTenantsAgainstTokens_SeesATenantBehindASharedUserTokenName(t *testing.T) {
	_, ctx, s, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	base := time.Now().Add(-time.Hour)
	for i, tenant := range []string{"acme", "zeta"} {
		if _, err := s.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: fmt.Sprintf("ot-shared-%d", i), Name: "u-alice", TenantID: tenant, Subject: "alice",
			TokenHash: fmt.Sprintf("h-shared-%d", i), AllowedScopes: []string{"runs:read"},
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed %s: %v", tenant, err)
		}
	}
	err := CheckPrincipalTenantsAgainstTokens(ctx, s, []string{"Acme"})
	if err == nil || !strings.Contains(err.Error(), "differs only by case") {
		t.Errorf("err = %v, want a case-collision error against acme", err)
	}
}

func TestOperatorTokenDef_RefusesDuplicateLiveName(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	mustOp(t, tool, ctx, `{"op":"create","name":"dup","tenant_id":"t","scopes":["runs:create"]}`)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"dup","tenant_id":"t","scopes":["runs:create"]}`))
	if !res.IsError || !strings.Contains(res.Text, "rotate") {
		t.Errorf("second create on a live name should refuse and suggest rotate; got %s", res.Text)
	}
}

func TestOperatorTokenDef_RotateMintsNewTokenAndGracesPrior(t *testing.T) {
	tool, ctx, s, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	c1 := mustOp(t, tool, ctx, `{"op":"create","name":"svc","tenant_id":"acme","scopes":["runs:create"]}`)
	tok1 := c1["token"].(string)
	prior := c1["def_id"].(string)

	c2 := mustOp(t, tool, ctx, `{"op":"rotate","name":"svc"}`)
	tok2 := c2["token"].(string)
	if tok1 == tok2 {
		t.Fatal("rotate returned the same token")
	}
	if c2["rotated_from"] != prior {
		t.Errorf("rotated_from = %v, want %s", c2["rotated_from"], prior)
	}
	if _, ok := c2["prior_retires_at"].(string); !ok {
		t.Error("rotate response missing prior_retires_at (grace window)")
	}
	// The prior row now has a future retired_at; the new row is current.
	priorRow, _ := s.OperatorTokenDefGet(ctx, prior)
	if priorRow.RetiredAt.IsZero() {
		t.Error("prior token should have a retired_at set after rotate")
	}
	cur, err := s.OperatorTokenDefGetCurrentByName(ctx, "svc")
	if err != nil || cur.DefID == prior {
		t.Errorf("current-by-name should be the NEW token after rotate (err=%v)", err)
	}
	// Rotation preserves the principal + scopes.
	if cur.TenantID != "acme" || len(cur.AllowedScopes) != 1 || cur.AllowedScopes[0] != "runs:create" {
		t.Errorf("rotate did not preserve principal/scopes: %+v", cur)
	}
	// History lists both.
	list := mustOp(t, tool, ctx, `{"op":"list","name":"svc"}`)
	if toks, _ := list["tokens"].([]any); len(toks) != 2 {
		t.Errorf("list should show 2 tokens after one rotate; got %d", len(toks))
	}
}

func TestOperatorTokenDef_RetireImmediate(t *testing.T) {
	tool, ctx, s, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	// Non-admin scope so the no-fail-open guard (which protects the LAST
	// admin token under no legacy fallback) doesn't apply — this test is
	// about retire taking effect immediately, not the admin-count guard.
	c := mustOp(t, tool, ctx, `{"op":"create","name":"temp","tenant_id":"t","scopes":["runs:create"]}`)
	defID := c["def_id"].(string)
	mustOp(t, tool, ctx, `{"op":"retire","name":"temp"}`)
	if _, err := s.OperatorTokenDefGetCurrentByName(ctx, "temp"); err == nil {
		t.Error("retired name should have no current token")
	}
	row, _ := s.OperatorTokenDefGet(ctx, defID)
	if row.RetiredAt.IsZero() {
		t.Error("retire should set retired_at")
	}
}

// ttl_seconds on create gives the token a fixed end, shown on the row.
func TestOperatorTokenDef_CreateWithTTLStoresTheExpiry(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()

	before := time.Now()
	out := mustOp(t, tool, ctx, `{"op":"create","name":"ci","tenant_id":"acme","scopes":["runs:create"],"ttl_seconds":600}`)
	if out["expires_at"] == nil || out["expired"] != false {
		t.Fatalf("create response = %v, want expires_at and expired=false", out)
	}
	row, err := st.OperatorTokenDefGet(ctx, out["def_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if d := row.ExpiresAt.Sub(before); d < 599*time.Second || d > 601*time.Second {
		t.Errorf("stored expiry is %s after the create, want ten minutes", d)
	}
	plain := mustOp(t, tool, ctx, `{"op":"create","name":"forever","tenant_id":"acme","scopes":["runs:create"]}`)
	if _, has := plain["expires_at"]; has {
		t.Errorf("a create without ttl_seconds carries expires_at: %v", plain)
	}
}

// An admin token that expires locks the deployment out on a date: one admin
// token turns the legacy login off, and when the last one runs out nothing can
// mint its replacement.
func TestOperatorTokenDef_TTLRefusedOnAnAdminToken(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	for _, in := range []string{
		`{"op":"create","name":"root","tenant_id":"acme","scopes":["substrate:admin"],"ttl_seconds":600}`,
		`{"op":"create","name":"z","tenant_id":"acme","scopes":["runs:create"],"ttl_seconds":0}`,
		`{"op":"create","name":"n","tenant_id":"acme","scopes":["runs:create"],"ttl_seconds":-1}`,
	} {
		res, err := tool.Execute(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Text, "ttl_seconds") {
			t.Errorf("%s: is_error=%v text=%q, want a refusal naming ttl_seconds", in, res.IsError, res.Text)
		}
	}
	if names, _ := st.OperatorTokenDefListNames(ctx); len(names) != 0 {
		t.Errorf("%d token(s) written by refused creates", len(names))
	}
}

// A rotation replaces the secret. It does not give the token more time.
func TestOperatorTokenDef_RotateKeepsTheExpiry(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	created := mustOp(t, tool, ctx, `{"op":"create","name":"ci","tenant_id":"acme","scopes":["runs:create"],"ttl_seconds":600}`)
	orig, _ := st.OperatorTokenDefGet(ctx, created["def_id"].(string))
	time.Sleep(5 * time.Millisecond)
	rotated := mustOp(t, tool, ctx, `{"op":"rotate","name":"ci"}`)
	next, err := st.OperatorTokenDefGet(ctx, rotated["def_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !next.ExpiresAt.Equal(orig.ExpiresAt) {
		t.Errorf("rotated token expires %v, the original %v — a rotation extended its life", next.ExpiresAt, orig.ExpiresAt)
	}
}

// Rotating a token that has already run out would mint one dead on arrival.
func TestOperatorTokenDef_RotateOfAnExpiredTokenIsRefused(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_dead", Name: "dead", TenantID: "acme", Subject: "s", TokenHash: "h-dead",
		AllowedScopes: []string{"runs:create"}, ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"rotate","def_id":"def_dead"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "expired") {
		t.Errorf("rotate of an expired token: is_error=%v text=%q, want a refusal", res.IsError, res.Text)
	}
	// Its name is free again: an expired token is not the name's live one.
	mustOp(t, tool, ctx, `{"op":"create","name":"dead","tenant_id":"acme","scopes":["runs:create"]}`)
}

// Names behind the per-user prefix are written by tenant operators and repeat
// across tenants, so the operator tool neither creates there nor acts by name
// there: "the current token named u-alice" is whichever tenant minted last.
func TestOperatorTokenDef_StaysOutOfThePerUserTokenNamespace(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	for _, tenant := range []string{"t1", "t2"} {
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: "def_" + tenant, Name: "u-alice", TenantID: tenant, Subject: "alice",
			TokenHash: "h-" + tenant, AllowedScopes: []string{auth.ScopeUser},
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, in := range []string{
		`{"op":"create","name":"u-new","tenant_id":"acme","scopes":["runs:create"]}`,
		`{"op":"rotate","name":"u-alice"}`,
		`{"op":"retire","name":"u-alice"}`,
	} {
		res, err := tool.Execute(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Text, "per-user") {
			t.Errorf("%s: is_error=%v text=%q, want a refusal", in, res.IsError, res.Text)
		}
	}
	for _, id := range []string{"def_t1", "def_t2"} {
		if row, _ := st.OperatorTokenDefGet(ctx, id); !row.RetiredAt.IsZero() {
			t.Errorf("%s was retired by a by-name operation", id)
		}
	}
	// By def_id the operator still reaches exactly the token it names.
	mustOp(t, tool, ctx, `{"op":"retire","def_id":"def_t1"}`)
	if row, _ := st.OperatorTokenDefGet(ctx, "def_t2"); !row.RetiredAt.IsZero() {
		t.Error("retiring def_t1 by id touched def_t2")
	}
}

// agents on create limits the token; a rotation carries the limit over; and
// the mistakes a mint must not guess at are refused with nothing written.
func TestOperatorTokenDef_AgentsRestrictionIsStoredAndSurvivesRotation(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()

	out := mustOp(t, tool, ctx, `{"op":"create","name":"ci","tenant_id":"acme","scopes":["runs:create"],"agents":["score","review"]}`)
	row, err := st.OperatorTokenDefGet(ctx, out["def_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	r, err := auth.ParseRestrictions(row.RestrictionsJSON)
	if err != nil || len(r.Agents) != 2 || !r.AgentAllowed("score") || r.AgentAllowed("judge") {
		t.Fatalf("stored restrictions = %q → %+v (err %v), want [score review]", row.RestrictionsJSON, r, err)
	}
	if out["restrictions"] == nil {
		t.Errorf("create response does not show the restrictions: %v", out)
	}

	rotated := mustOp(t, tool, ctx, `{"op":"rotate","name":"ci"}`)
	next, err := st.OperatorTokenDefGet(ctx, rotated["def_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if nr, err := auth.ParseRestrictions(next.RestrictionsJSON); err != nil || !nr.RunPlaneOnly() || nr.AgentAllowed("judge") {
		t.Errorf("rotated token restrictions = %q (err %v): a rotation dropped the limit", next.RestrictionsJSON, err)
	}

	plain := mustOp(t, tool, ctx, `{"op":"create","name":"free","tenant_id":"acme","scopes":["runs:create"]}`)
	if row, _ := st.OperatorTokenDefGet(ctx, plain["def_id"].(string)); row.RestrictionsJSON != "" {
		t.Errorf("a create without agents stored %q", row.RestrictionsJSON)
	}

	before, _ := st.OperatorTokenDefListNames(ctx)
	for _, in := range []string{
		`{"op":"create","name":"e","tenant_id":"acme","scopes":["runs:create"],"agents":[]}`,
		`{"op":"create","name":"b","tenant_id":"acme","scopes":["runs:create"],"agents":["has space"]}`,
		`{"op":"create","name":"root2","tenant_id":"acme","scopes":["substrate:admin"],"agents":["score"]}`,
	} {
		res, err := tool.Execute(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Text, "agents") {
			t.Errorf("%s: is_error=%v text=%q, want a refusal naming agents", in, res.IsError, res.Text)
		}
	}
	if after, _ := st.OperatorTokenDefListNames(ctx); len(after) != len(before) {
		t.Errorf("refused creates wrote %d token name(s)", len(after)-len(before))
	}
}

// overrides on create locks the per-run settings, survives a rotation, and is
// refused when it is not a policy.
func TestOperatorTokenDef_OverridesLockIsStoredAndSurvivesRotation(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	policyOf := func(defID string) *auth.OverridePolicy {
		row, err := st.OperatorTokenDefGet(ctx, defID)
		if err != nil {
			t.Fatal(err)
		}
		r, err := auth.ParseRestrictions(row.RestrictionsJSON)
		if err != nil {
			t.Fatalf("stored %q: %v", row.RestrictionsJSON, err)
		}
		return r.Overrides
	}
	none := mustOp(t, tool, ctx, `{"op":"create","name":"locked","tenant_id":"acme","scopes":["runs:create"],"overrides":"none"}`)
	if p := policyOf(none["def_id"].(string)); p == nil || len(p.Allow) != 0 {
		t.Errorf(`"none" stored as %+v`, p)
	}
	some := mustOp(t, tool, ctx, `{"op":"create","name":"some","tenant_id":"acme","scopes":["runs:create"],"overrides":["effort"]}`)
	if p := policyOf(some["def_id"].(string)); p == nil || len(p.Allow) != 1 || p.Allow[0] != "effort" {
		t.Errorf("list stored as %+v", p)
	}
	rotated := mustOp(t, tool, ctx, `{"op":"rotate","name":"locked"}`)
	if p := policyOf(rotated["def_id"].(string)); p == nil {
		t.Error("a rotation dropped the override lock")
	}
	plain := mustOp(t, tool, ctx, `{"op":"create","name":"plain","tenant_id":"acme","scopes":["runs:create"]}`)
	if p := policyOf(plain["def_id"].(string)); p != nil {
		t.Errorf("a create without overrides stored a policy: %+v", p)
	}
	for _, in := range []string{
		`{"op":"create","name":"x1","tenant_id":"acme","scopes":["runs:create"],"overrides":"all"}`,
		`{"op":"create","name":"x2","tenant_id":"acme","scopes":["runs:create"],"overrides":[]}`,
		`{"op":"create","name":"x3","tenant_id":"acme","scopes":["runs:create"],"overrides":["temperature"]}`,
	} {
		res, err := tool.Execute(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Text, "overrides") {
			t.Errorf("%s: is_error=%v text=%q, want a refusal naming overrides", in, res.IsError, res.Text)
		}
	}
}

func TestOperatorTokenDef_RateIsStoredAndSurvivesRotation(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	rateOf := func(defID string) *auth.RateLimit {
		row, err := st.OperatorTokenDefGet(ctx, defID)
		if err != nil {
			t.Fatal(err)
		}
		r, err := auth.ParseRestrictions(row.RestrictionsJSON)
		if err != nil {
			t.Fatalf("stored %q: %v", row.RestrictionsJSON, err)
		}
		return r.Rate
	}
	out := mustOp(t, tool, ctx, `{"op":"create","name":"ci","tenant_id":"acme","scopes":["runs:create"],"rate":{"per_minute":6,"burst":20}}`)
	if r := rateOf(out["def_id"].(string)); r == nil || r.PerMinute != 6 || r.Burst != 20 {
		t.Errorf("stored rate = %+v", r)
	}
	rotated := mustOp(t, tool, ctx, `{"op":"rotate","name":"ci"}`)
	if r := rateOf(rotated["def_id"].(string)); r == nil || r.PerMinute != 6 {
		t.Errorf("a rotation dropped the rate: %+v", r)
	}
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad","tenant_id":"acme","scopes":["runs:create"],"rate":{"per_minute":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "rate") {
		t.Errorf("a zero rate: is_error=%v text=%q, want a refusal naming rate", res.IsError, res.Text)
	}
}

func TestOperatorTokenDef_RetentionIsStoredAndSurvivesRotation(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	retentionOf := func(defID string) string {
		t.Helper()
		row, err := st.OperatorTokenDefGet(ctx, defID)
		if err != nil {
			t.Fatal(err)
		}
		r, err := auth.ParseRestrictions(row.RestrictionsJSON)
		if err != nil {
			t.Fatalf("stored %q: %v", row.RestrictionsJSON, err)
		}
		return r.Retention
	}
	out := mustOp(t, tool, ctx, `{"op":"create","name":"quiet","tenant_id":"acme","scopes":["runs:create"],"retention":"none"}`)
	if got := retentionOf(out["def_id"].(string)); got != retentionmode.None {
		t.Errorf("stored retention = %q, want none", got)
	}
	rotated := mustOp(t, tool, ctx, `{"op":"rotate","name":"quiet"}`)
	if got := retentionOf(rotated["def_id"].(string)); got != retentionmode.None {
		t.Errorf("a rotation dropped the mode: %q", got)
	}
	plain := mustOp(t, tool, ctx, `{"op":"create","name":"plain","tenant_id":"acme","scopes":["runs:create"],"retention":"default"}`)
	if got := retentionOf(plain["def_id"].(string)); got != "" {
		t.Errorf("retention default stored as %q, want no setting", got)
	}
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad","tenant_id":"acme","scopes":["runs:create"],"retention":"nonee"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "retention") {
		t.Errorf("a misspelled retention: is_error=%v text=%q, want a refusal naming retention", res.IsError, res.Text)
	}
}

// RFC ES D2: with a derived pepper, both mint paths (create and rotate)
// store the tagged derived hash the auth lookup computes.
func TestOperatorTokenDef_MintsUnderTheDerivedPepper(t *testing.T) {
	tool, ctx, s, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	tool.Hasher = auth.NewTokenHasher("", "derived-pepper", "")

	out := mustOp(t, tool, ctx, `{"op":"create","name":"dp","tenant_id":"acme","subject":"dp","scopes":["runs:create"]}`)
	plaintext, _ := out["token"].(string)
	defID, _ := out["def_id"].(string)
	row, err := s.OperatorTokenDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if row.TokenHash != tool.Hasher.Hash(plaintext) || tool.Hasher.Form(row.TokenHash) != auth.FormCurrent {
		t.Errorf("create: stored hash is not the derived-pepper hash")
	}

	out = mustOp(t, tool, ctx, `{"op":"rotate","name":"dp"}`)
	plaintext, _ = out["token"].(string)
	defID, _ = out["def_id"].(string)
	row, err = s.OperatorTokenDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("store get rotated: %v", err)
	}
	if row.TokenHash != tool.Hasher.Hash(plaintext) {
		t.Errorf("rotate: stored hash is not the derived-pepper hash")
	}
}
