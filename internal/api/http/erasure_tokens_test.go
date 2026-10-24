package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// erasureTokenFixture is a token-auth server with the auth cache ON, so a
// revocation is only seen if the erasure flushes it, and alice holding one
// minted member token bound to an ACL list.
func erasureTokenFixture(t *testing.T) (*Server, store.Store, context.Context, mintUserTokenResponse, *recordingSink) {
	t.Helper()
	s, st := tokenAuthServer(t, "legacy")
	s.EnableTokenCache(time.Hour)
	s.SetEraseAudit(&recordingSink{})
	tokenLog := &recordingSink{}
	s.SetTokenAudit(tokenLog)
	ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	minted := mintFor(t, s, ops, "alice", "")
	ctx := auth.WithPrincipal(context.Background(), ops)
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "token-list", SubjectKind: "token", SubjectID: minted.DefID}); err != nil {
		t.Fatal(err)
	}
	// Resolve once so the principal sits in the cache: the test then proves
	// the erasure flushed it, not that nothing was cached.
	if _, ok := s.resolvePrincipal(ctx, minted.Token); !ok {
		t.Fatal("precondition: the minted token does not authenticate")
	}
	tokenLog.events = nil
	return s, st, ctx, minted, tokenLog
}

func tokenBindings(t *testing.T, st store.Store, defID string) int {
	t.Helper()
	rows, err := st.AclBindingList(context.Background(), "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.SubjectKind == "token" && r.SubjectID == defID {
			n++
		}
	}
	return n
}

// TestErasure_RevokedBearerIsRefusedAfterErasure: erasing a subject retires
// its member tokens, so the bearer stops authenticating at once — through a
// warm auth cache — and the bindings keyed to the token's def id go with it.
// The response names the token by def id and name, never by its value.
func TestErasure_RevokedBearerIsRefusedAfterErasure(t *testing.T) {
	s, st, ctx, minted, tokenLog := erasureTokenFixture(t)

	res, err := s.ErasureExecute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.resolvePrincipal(ctx, minted.Token); ok {
		t.Error("the erased subject's bearer still authenticates")
	}
	if res.Deleted["operator_tokens"] != 1 || res.Deleted["acl_token_bindings"] != 1 {
		t.Errorf("deleted = %v, want operator_tokens 1, acl_token_bindings 1", res.Deleted)
	}
	if len(res.Access.Tokens) != 1 || res.Access.Tokens[0].DefID != minted.DefID || res.Access.Tokens[0].Name != minted.Name {
		t.Errorf("access.tokens = %+v, want [%s %s]", res.Access.Tokens, minted.DefID, minted.Name)
	}
	if !contains(res.Access.Lists, "token-list") {
		t.Errorf("access.lists = %v, want the token binding's list", res.Access.Lists)
	}
	if n := tokenBindings(t, st, minted.DefID); n != 0 {
		t.Errorf("%d binding(s) to the revoked token survived", n)
	}
	if len(tokenLog.events) == 0 || tokenLog.events[0].Action != "retire" || tokenLog.events[0].TargetDefID != minted.DefID {
		t.Errorf("token audit = %+v, want a retire of %s", tokenLog.events, minted.DefID)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), minted.Token) {
		t.Fatal("the erasure response carries the bearer")
	}

	// Idempotent: a re-run revokes nothing more, and still clears a binding
	// left on the already-retired token.
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "late", SubjectKind: "token", SubjectID: minted.DefID}); err != nil {
		t.Fatal(err)
	}
	again, err := s.ErasureExecute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Deleted["operator_tokens"] != 0 || len(again.Access.Tokens) != 0 || again.Deleted["acl_token_bindings"] != 1 {
		t.Errorf("re-run deleted = %v tokens = %+v, want 0 tokens and the late binding", again.Deleted, again.Access.Tokens)
	}
}

// TestErasure_DryRunRevokesNoToken: a dry run names the token it would
// revoke and leaves it — and its bindings — working.
func TestErasure_DryRunRevokesNoToken(t *testing.T) {
	s, st, ctx, minted, tokenLog := erasureTokenFixture(t)

	res, err := s.ErasureExecute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.resolvePrincipal(ctx, minted.Token); !ok {
		t.Error("a dry run revoked the bearer")
	}
	if row, _ := st.OperatorTokenDefGet(ctx, minted.DefID); !row.RetiredAt.IsZero() {
		t.Error("a dry run retired the token row")
	}
	if n := tokenBindings(t, st, minted.DefID); n != 1 {
		t.Errorf("a dry run removed the token's binding (%d left)", n)
	}
	if res.Deleted["operator_tokens"] != 1 || len(res.Access.Tokens) != 1 || res.Access.Tokens[0].DefID != minted.DefID {
		t.Errorf("dry run deleted = %v tokens = %+v, want the token named", res.Deleted, res.Access.Tokens)
	}
	if len(tokenLog.events) != 0 {
		t.Errorf("a dry run wrote token audit records: %+v", tokenLog.events)
	}
}

// TestErasure_LeavesAPrivilegedTokenAndSaysSo: a tenant-operator or admin
// token that shares the subject is a role, not the member's credential —
// the member-token rule user delete uses — so it is not revoked; erasure
// says which one is left and how to retire it.
func TestErasure_LeavesAPrivilegedTokenAndSaysSo(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	s.SetEraseAudit(&recordingSink{})
	seedToken(t, st, "lc_privileged_for_alice", "acme", "alice", []string{auth.ScopeTenant}, time.Time{})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})

	rep, err := s.ErasureReport(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := rep.Tier2.Counts["operator_tokens"]; !ok || n != 0 {
		t.Errorf("report operator_tokens = %d (%v), want 0, examined", n, ok)
	}
	res, err := s.ErasureExecute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := st.OperatorTokenDefGet(ctx, "def_alice"); !row.RetiredAt.IsZero() {
		t.Error("erasure retired a tenant-operator token")
	}
	if r := res.Retained["operator_tokens_privileged"]; !strings.Contains(r, "def_alice") {
		t.Errorf("retained = %v, want the privileged token named", res.Retained)
	}
}
