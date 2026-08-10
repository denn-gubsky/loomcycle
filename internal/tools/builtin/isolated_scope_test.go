package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC BX P2b: the data tools confine an ISOLATED run (RunIdentity.Isolated, set
// from a substrate:user principal) to its own user/agent scope — the
// tenant-shared and cross-tenant global keyspaces are refused for read + write.
// A non-isolated run behaves exactly as before (regression). These exercise the
// scope-resolution choke point of each tool directly (no store needed); the
// refusal error carries "isolated" so a policy refusal can't masquerade as one.

// isoScopeCtx builds a run ctx with the given isolation bit, an agent name, and a
// user_id/tenant so the own-scope (user/agent) resolutions succeed.
func isoScopeCtx(isolated bool) context.Context {
	ctx := tools.WithAgentName(context.Background(), "agent-1")
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID: "alice", AgentID: "a_test", TenantID: "acme", Isolated: isolated,
	})
}

func TestMemory_IsolatedConfinesScope(t *testing.T) {
	m := &Memory{}
	// The agent GRANTS tenant so the refusal proves ConfineIsolatedScope fires,
	// not the memory_scopes ACL.
	policy := tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}

	iso := tools.WithMemoryPolicy(isoScopeCtx(true), policy)
	if _, _, err := m.resolveScope(iso, "tenant"); err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Errorf("isolated resolveScope(tenant) err=%v, want isolated-confinement refusal", err)
	}
	for _, sc := range []string{"user", "agent"} {
		if _, _, err := m.resolveScope(iso, sc); err != nil {
			t.Errorf("isolated resolveScope(%s) err=%v, want ok (own scope)", sc, err)
		}
	}

	// Non-isolated: tenant resolves fine (regression — unchanged behaviour).
	non := tools.WithMemoryPolicy(isoScopeCtx(false), policy)
	if _, _, err := m.resolveScope(non, "tenant"); err != nil {
		t.Errorf("non-isolated resolveScope(tenant) err=%v, want ok", err)
	}
}

func TestPath_IsolatedConfinesScope(t *testing.T) {
	p := &Path{}

	iso := isoScopeCtx(true)
	if _, _, _, err := p.resolveScope(iso, "tenant"); err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Errorf("isolated Path.resolveScope(tenant) err=%v, want isolated-confinement refusal", err)
	}
	for _, sc := range []string{"user", "agent"} {
		if _, _, _, err := p.resolveScope(iso, sc); err != nil {
			t.Errorf("isolated Path.resolveScope(%s) err=%v, want ok (own scope)", sc, err)
		}
	}

	non := isoScopeCtx(false)
	if _, _, _, err := p.resolveScope(non, "tenant"); err != nil {
		t.Errorf("non-isolated Path.resolveScope(tenant) err=%v, want ok", err)
	}
}

func TestDocument_IsolatedConfinesScope(t *testing.T) {
	d := &Document{}
	// Grant tenant on BOTH planes so the isolated refusal proves ConfineIsolatedScope
	// fires BEFORE (and regardless of) the tenant grant check.
	grant := func(ctx context.Context) context.Context {
		ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}})
		return tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}})
	}

	iso := grant(isoScopeCtx(true))
	if _, _, err := d.resolveScope(iso, "tenant"); err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Errorf("isolated Document.resolveScope(tenant) err=%v, want isolated-confinement refusal even with grants", err)
	}
	if _, _, err := d.resolveScope(iso, "user"); err != nil {
		t.Errorf("isolated Document.resolveScope(user) err=%v, want ok (own scope)", err)
	}

	non := grant(isoScopeCtx(false))
	if _, _, err := d.resolveScope(non, "tenant"); err != nil {
		t.Errorf("non-isolated Document.resolveScope(tenant) err=%v, want ok (granted)", err)
	}
}

func TestChannel_IsolatedConfinesScope(t *testing.T) {
	c := &Channel{}
	// The channel is on the publish allowlist so the refusal proves
	// ConfineIsolatedScope fires, not the channel ACL.
	policy := tools.ChannelPolicyValue{
		Publish: []string{"team", "world", "mine"},
		Channels: map[string]tools.ChannelDef{
			"team":  {Name: "team", Scope: "tenant"},
			"world": {Name: "world", Scope: "global"},
			"mine":  {Name: "mine", Scope: "user"},
		},
	}

	iso := isoScopeCtx(true)
	for _, name := range []string{"team", "world"} {
		if _, _, _, err := c.resolveChannel(iso, policy, "publish", name); err == nil || !strings.Contains(err.Error(), "isolated") {
			t.Errorf("isolated resolveChannel(%s) err=%v, want isolated-confinement refusal", name, err)
		}
	}
	if _, _, _, err := c.resolveChannel(iso, policy, "publish", "mine"); err != nil {
		t.Errorf("isolated resolveChannel(mine=user) err=%v, want ok (own scope)", err)
	}

	non := isoScopeCtx(false)
	for _, name := range []string{"team", "world"} {
		if _, _, _, err := c.resolveChannel(non, policy, "publish", name); err != nil {
			t.Errorf("non-isolated resolveChannel(%s) err=%v, want ok", name, err)
		}
	}
}
