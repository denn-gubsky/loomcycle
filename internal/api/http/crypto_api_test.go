package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// cryptoAPIServer is a token-auth server with the Crypto tool on a credential
// store, and the handler behind the real auth middleware.
func cryptoAPIServer(t *testing.T, maxOpsPerRun int) (http.Handler, *Server, store.Store, *credential.Engine) {
	t.Helper()
	s, st := tokenAuthServer(t, "legacy-crypto-token")
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	s.tools = []tools.Tool{&builtin.Crypto{Engine: engine, MaxOpsPerRun: maxOpsPerRun}}
	s.runRate = newRunRateLimiter(nil)
	return s.authMiddleware(http.HandlerFunc(s.handleSubstrateCrypto)), s, st, engine
}

func putCryptoKey(t *testing.T, e *credential.Engine, tenant, scope, scopeID, name string, fill byte) {
	t.Helper()
	m := make([]byte, 32)
	for i := range m {
		m[i] = fill
	}
	if _, err := e.PutCryptoKey(context.Background(), credential.Identity{TenantID: tenant, Scope: scope, ScopeID: scopeID, Name: name}, "aes-256-gcm", m, nil); err != nil {
		t.Fatal(err)
	}
}

// postCrypto sends one call and returns the status and the decoded body.
func postCrypto(t *testing.T, h http.Handler, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/_crypto", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A tenant operator encrypts with its tenant's key and decrypts what it made;
// the ciphertext is the in-run tool's own, so the tenant's runs open it.
func TestCryptoAPI_TenantOperatorUsesItsTenantKeyAndRunsOpenIt(t *testing.T) {
	h, s, st, e := cryptoAPIServer(t, 0)
	seedToken(t, st, "lct_acme_op", "acme", "alice", []string{auth.ScopeTenant}, time.Time{})
	putCryptoKey(t, e, "acme", "tenant", "", "fields", 7)

	code, out := postCrypto(t, h, "lct_acme_op", `{"op":"encrypt","key":"fields","plaintext":"card 4111","context":"customer:42"}`)
	if code != http.StatusOK {
		t.Fatalf("encrypt = %d %v", code, out)
	}
	ct, _ := out["ciphertext"].(string)
	if !strings.HasPrefix(ct, "lcx1:kt:fields:1:") {
		t.Fatalf("ciphertext %q is not the tenant key's envelope", ct)
	}
	code, out = postCrypto(t, h, "lct_acme_op", `{"op":"decrypt","ciphertext":"`+ct+`","context":"customer:42"}`)
	if code != http.StatusOK || out["plaintext"] != "card 4111" {
		t.Fatalf("decrypt = %d %v", code, out)
	}

	// The same tenant's run decrypts it with the tool.
	runCtx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "u9", AgentID: "a_1"})
	runCtx = tools.WithRunID(runCtx, "run_1")
	runCtx = tools.UpdateRunPolicy(runCtx, func(rp *tools.RunPolicy) { rp.Crypto = tools.CryptoPolicyValue{Scopes: []string{"tenant"}} })
	res, err := s.tools[0].Execute(runCtx, json.RawMessage(`{"op":"decrypt","ciphertext":"`+ct+`","context":"customer:42"}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, "card 4111") {
		t.Fatalf("in-run decrypt of the API's ciphertext = %+v %v", res, err)
	}
}

// Only operators: a tenant's members, an isolated user and a run-plane token
// are refused before anything runs.
func TestCryptoAPI_RefusesMembersUsersAndRunTokens(t *testing.T) {
	h, _, st, e := cryptoAPIServer(t, 0)
	putCryptoKey(t, e, "acme", "tenant", "", "fields", 7)
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}
	seedToken(t, st, "lct_member", "acme", "mia", member, time.Time{})
	seedToken(t, st, "lct_isolated", "acme", "ivy", []string{auth.ScopeUser}, time.Time{})
	seedToken(t, st, "lct_runner", "acme", "rex", []string{auth.ScopeRunsCreate}, time.Time{})
	for _, tok := range []string{"lct_member", "lct_isolated", "lct_runner"} {
		if code, out := postCrypto(t, h, tok, `{"op":"encrypt","key":"fields","plaintext":"x"}`); code != http.StatusForbidden {
			t.Errorf("%s: %d %v, want 403", tok, code, out)
		}
	}
}

// The caller's own subject's user keys, and never an agent's (its runs' own)
// or another user's.
func TestCryptoAPI_KeysAreTheTenantsAndTheCallersOwn(t *testing.T) {
	h, _, st, e := cryptoAPIServer(t, 0)
	seedToken(t, st, "lct_alice", "acme", "alice", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, st, "lct_bob", "acme", "bob", []string{auth.ScopeTenant}, time.Time{})
	putCryptoKey(t, e, "acme", "user", "alice", "mine", 1)
	putCryptoKey(t, e, "acme", "agent", "bot", "botkey", 2)

	if code, out := postCrypto(t, h, "lct_alice", `{"op":"encrypt","key":"mine","plaintext":"x"}`); code != http.StatusOK || !strings.HasPrefix(out["ciphertext"].(string), "lcx1:ku:mine:1:") {
		t.Fatalf("alice with her own key = %d %v", code, out)
	}
	if code, out := postCrypto(t, h, "lct_bob", `{"op":"encrypt","key":"mine","plaintext":"x"}`); code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "no key") {
		t.Errorf("bob with alice's key name = %d %v, want not found", code, out)
	}
	if code, out := postCrypto(t, h, "lct_alice", `{"op":"encrypt","key":"botkey","key_scope":"agent","plaintext":"x"}`); code != http.StatusUnprocessableEntity || out["errorCategory"] != "permission" ||
		!strings.Contains(out["error"].(string), "outside a run") {
		t.Errorf("an agent's key pinned = %d %v, want a permission refusal that speaks of outside a run", code, out)
	}
	if code, out := postCrypto(t, h, "lct_alice", `{"op":"encrypt","key":"botkey","plaintext":"x"}`); code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "no key") {
		t.Errorf("an agent's key = %d %v, want not found", code, out)
	}
	if code, out := postCrypto(t, h, "lct_alice", `{"op":"encrypt","run":"anything","key_type":"aes-256-gcm","plaintext":"x"}`); code != http.StatusUnprocessableEntity || out["errorCategory"] != "permission" {
		t.Errorf("a run credential outside a run = %d %v, want a permission refusal", code, out)
	}
}

// Only an admin names another tenant; it then gets that tenant's keys, and the
// ciphertext is that tenant's (its runs open it, the admin's own tenant's do not).
func TestCryptoAPI_TenantFieldIsAdminOnly(t *testing.T) {
	h, _, st, e := cryptoAPIServer(t, 0)
	seedToken(t, st, "lct_acme_op", "acme", "alice", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, st, "lct_admin", "ops", "root", []string{auth.ScopeAdmin}, time.Time{})
	putCryptoKey(t, e, "globex", "tenant", "", "fields", 9)

	if code, out := postCrypto(t, h, "lct_acme_op", `{"op":"encrypt","key":"fields","plaintext":"x","tenant":"globex"}`); code != http.StatusUnprocessableEntity || out["errorCategory"] != "permission" {
		t.Fatalf("a tenant operator naming another tenant = %d %v, want a permission refusal", code, out)
	}
	if code, out := postCrypto(t, h, "lct_acme_op", `{"op":"hash","data":"x","tenant":"acme"}`); code != http.StatusOK {
		t.Errorf("naming its own tenant = %d %v, want it accepted", code, out)
	}
	code, out := postCrypto(t, h, "lct_admin", `{"op":"encrypt","key":"fields","plaintext":"x","tenant":"globex"}`)
	if code != http.StatusOK {
		t.Fatalf("admin naming globex = %d %v", code, out)
	}
	ct := out["ciphertext"].(string)
	if code, out := postCrypto(t, h, "lct_admin", `{"op":"decrypt","ciphertext":"`+ct+`","tenant":"globex"}`); code != http.StatusOK || out["plaintext"] != "x" {
		t.Errorf("admin decrypt in globex = %d %v", code, out)
	}
	if code, _ := postCrypto(t, h, "lct_admin", `{"op":"encrypt","key":"fields","plaintext":"x"}`); code == http.StatusOK {
		t.Error("admin without tenant used globex's key")
	}
	if code, out := postCrypto(t, h, "lct_admin", `{"op":"hash","data":"x","tenant":7}`); code != http.StatusUnprocessableEntity || out["errorCategory"] != "validation" {
		t.Errorf("a non-string tenant = %d %v, want a validation refusal", code, out)
	}
}

// The audit record names who asked, not the tenant whose key was used: an
// admin using another tenant's key is recorded as the admin, with that tenant
// as the target.
func TestCryptoAPI_AuditRecordsTheCallerAsTheActor(t *testing.T) {
	h, s, st, e := cryptoAPIServer(t, 0)
	sink := &recordingSink{}
	s.tools[0].(*builtin.Crypto).Audit = sink
	seedToken(t, st, "lct_admin_audit", "ops", "root", []string{auth.ScopeAdmin}, time.Time{})
	putCryptoKey(t, e, "globex", "tenant", "", "fields", 9)

	if code, out := postCrypto(t, h, "lct_admin_audit", `{"op":"encrypt","key":"fields","plaintext":"x","tenant":"globex"}`); code != http.StatusOK {
		t.Fatalf("encrypt = %d %v", code, out)
	}
	if len(sink.events) != 1 {
		t.Fatalf("audit records = %d, want 1", len(sink.events))
	}
	ev := sink.events[0]
	if ev.ActorTenant != "ops" || ev.ActorSubject != "root" {
		t.Errorf("actor = %q/%q, want the admin ops/root", ev.ActorTenant, ev.ActorSubject)
	}
	if ev.TargetTenant != "globex" || ev.TargetName != "fields" || ev.CryptoOutcome != "ok" {
		t.Errorf("target = %q/%q outcome %q, want globex/fields ok", ev.TargetTenant, ev.TargetName, ev.CryptoOutcome)
	}
}

// No run, so no run's op budget; a rate on the token bounds the calls instead.
func TestCryptoAPI_TokenRateNotRunBudget(t *testing.T) {
	h, _, st, _ := cryptoAPIServer(t, 2)
	seedToken(t, st, "lct_acme_op", "acme", "alice", []string{auth.ScopeTenant}, time.Time{})
	for i := 0; i < 5; i++ {
		if code, out := postCrypto(t, h, "lct_acme_op", `{"op":"hash","data":"x"}`); code != http.StatusOK {
			t.Fatalf("call %d = %d %v: the run budget applied to the operator API", i+1, code, out)
		}
	}

	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_rated", Name: "rated", TenantID: "acme", Subject: "rita",
		TokenHash:        auth.HashToken(testTokenPepper, "lct_rated"),
		AllowedScopes:    []string{auth.ScopeTenant},
		RestrictionsJSON: `{"rate":{"per_minute":1,"burst":2}}`,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if code, out := postCrypto(t, h, "lct_rated", `{"op":"hash","data":"x"}`); code != http.StatusOK {
			t.Fatalf("rated call %d = %d %v", i+1, code, out)
		}
	}
	req := httptest.NewRequest("POST", "/v1/_crypto", strings.NewReader(`{"op":"hash","data":"x"}`))
	req.Header.Set("Authorization", "Bearer lct_rated")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("past the token's rate = %d (Retry-After %q), want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}
