package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
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
	tool := &OperatorTokenDef{Store: s, Pepper: testPepper, Audit: audit.NopSink{}}
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

func TestOperatorTokenDef_DefaultScopeIsAdmin(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	out := mustOp(t, tool, ctx, `{"op":"create","name":"root","tenant_id":"default"}`)
	scopes, _ := out["allowed_scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != auth.ScopeAdmin {
		t.Errorf("default scopes = %v, want [%s]", scopes, auth.ScopeAdmin)
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

func TestOperatorTokenDef_RefusesDuplicateLiveName(t *testing.T) {
	tool, ctx, _, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	mustOp(t, tool, ctx, `{"op":"create","name":"dup","tenant_id":"t"}`)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"dup","tenant_id":"t"}`))
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
	c := mustOp(t, tool, ctx, `{"op":"create","name":"temp","tenant_id":"t"}`)
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
