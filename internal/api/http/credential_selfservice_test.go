package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// TestConstrainToUserScope pins the RFC CN isolated-user rule: an isolated
// substrate:user caller's CredentialDef input is confined to scope=user — an
// omitted scope defaults to user (never the tool's tenant default), an explicit
// scope=user passes, and scope=tenant/agent is refused with credential_scope_forbidden.
func TestConstrainToUserScope(t *testing.T) {
	// Omitted scope → rewritten to user (must not fall through to the tool's tenant default).
	out, gerr := constrainToUserScope([]byte(`{"op":"list"}`))
	if gerr != nil {
		t.Fatalf("omitted scope: unexpected refusal %+v", gerr)
	}
	if !strings.Contains(string(out), `"scope":"user"`) {
		t.Errorf("omitted scope not defaulted to user: %s", out)
	}

	// Explicit scope=user → allowed, stays user.
	out, gerr = constrainToUserScope([]byte(`{"op":"create","name":"telegram","scope":"user","value":"x"}`))
	if gerr != nil || !strings.Contains(string(out), `"scope":"user"`) {
		t.Errorf("scope=user: out=%s gerr=%+v", out, gerr)
	}
	// The secret value survives the rewrite (the guard must not drop fields).
	if !strings.Contains(string(out), `"value":"x"`) {
		t.Errorf("scope=user rewrite dropped a field: %s", out)
	}

	// scope=tenant → refused.
	if _, gerr = constrainToUserScope([]byte(`{"op":"create","name":"t","scope":"tenant","value":"x"}`)); gerr == nil ||
		gerr.status != http.StatusForbidden || gerr.code != "credential_scope_forbidden" {
		t.Errorf("scope=tenant: want 403 credential_scope_forbidden, got %+v", gerr)
	}

	// scope=agent → refused.
	if _, gerr = constrainToUserScope([]byte(`{"op":"list","scope":"agent"}`)); gerr == nil {
		t.Error("scope=agent: want a guardError, got nil")
	}

	// A non-object body (array/scalar) passes through so the tool rejects it with
	// its own message rather than the guard masking it as a scope error.
	if _, gerr = constrainToUserScope([]byte(`[1,2,3]`)); gerr != nil {
		t.Errorf("non-object body should pass through, got %+v", gerr)
	}
}

// TestDispatchSubstrateCtxGuarded_AppliesGuard drives the guarded dispatch through
// an HTTP request with a capturing fake connector, proving the RFC CN plumbing:
// the guard's rewritten body is what reaches the tool, and a guard refusal 403s
// before the tool is ever called.
func TestDispatchSubstrateCtxGuarded_AppliesGuard(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")
	guard := func(_ context.Context, body []byte) ([]byte, *guardError) { return constrainToUserScope(body) }

	call := func(inBody string) (status int, forwarded string, called bool) {
		fake := func(_ context.Context, input json.RawMessage) (connector.ToolResult, error) {
			forwarded = string(input)
			called = true
			return connector.ToolResult{Text: `{"ok":true}`}, nil
		}
		req := httptest.NewRequest("POST", "/v1/_credentialdef", strings.NewReader(inBody))
		rec := httptest.NewRecorder()
		s.dispatchSubstrateCtxGuarded(rec, req, "CredentialDef", fake, substrateAdminUserCtx, guard)
		return rec.Code, forwarded, called
	}

	// Omitted scope: the tool receives scope=user, 200.
	code, fwd, called := call(`{"op":"create","name":"telegram","value":"x"}`)
	if code != http.StatusOK || !called {
		t.Fatalf("omitted scope: code=%d called=%v, want 200 + tool called", code, called)
	}
	if !strings.Contains(fwd, `"scope":"user"`) {
		t.Errorf("omitted scope not rewritten to user before the tool: %s", fwd)
	}

	// scope=tenant: refused 403, the tool is never called.
	code, _, called = call(`{"op":"create","name":"telegram","scope":"tenant","value":"x"}`)
	if code != http.StatusForbidden {
		t.Errorf("scope=tenant: code=%d, want 403", code)
	}
	if called {
		t.Error("scope=tenant reached the tool; the guard should refuse before dispatch")
	}
}
