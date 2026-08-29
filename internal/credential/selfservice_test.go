package credential

import (
	"strings"
	"testing"
)

// TestConstrainToUserScope pins the RFC CN transport-neutral rule shared by the
// HTTP handler and the MCP dispatch: an isolated caller's CredentialDef input is
// confined to scope=user — omitted defaults to user, user passes, tenant/agent is
// refused, and a non-object body passes through for the tool to reject.
func TestConstrainToUserScope(t *testing.T) {
	// Omitted scope → rewritten to user (never the tool's tenant default).
	out, refused := ConstrainToUserScope([]byte(`{"op":"list"}`))
	if refused {
		t.Fatal("omitted scope was refused, want defaulted to user")
	}
	if !strings.Contains(string(out), `"scope":"user"`) {
		t.Errorf("omitted scope not defaulted to user: %s", out)
	}

	// Explicit scope=user → allowed, other fields preserved.
	out, refused = ConstrainToUserScope([]byte(`{"op":"create","name":"telegram","scope":"user","value":"x"}`))
	if refused || !strings.Contains(string(out), `"scope":"user"`) || !strings.Contains(string(out), `"value":"x"`) {
		t.Errorf("scope=user: out=%s refused=%v", out, refused)
	}

	// scope=tenant and scope=agent → refused.
	for _, s := range []string{"tenant", "agent"} {
		if _, refused := ConstrainToUserScope([]byte(`{"op":"create","name":"t","scope":"` + s + `","value":"x"}`)); !refused {
			t.Errorf("scope=%s: want refused", s)
		}
	}

	// Non-object body → passthrough (tool rejects it).
	if _, refused := ConstrainToUserScope([]byte(`[1,2,3]`)); refused {
		t.Error("non-object body should pass through, not be refused")
	}
}
