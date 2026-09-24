package builtin

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Every tool with a `scope` argument reports its grants by running its OWN
// scope check over each value its schema offers. The report is therefore the
// enforcement itself, not a second reading of the agent's yaml, and cannot
// drift from what a call would be told: a refused scope carries the same error
// the call would return.
//
// All the checks called here are pure — they read the run context and the
// ctx-attached policies and touch no store — which is what makes calling them
// speculatively, once per scope value, safe.

func scopeGrants(schema json.RawMessage, check func(scope string) error) []tools.ScopeGrant {
	values := tools.SchemaEnum(schema, "scope")
	out := make([]tools.ScopeGrant, 0, len(values))
	for _, v := range values {
		g := tools.ScopeGrant{Scope: v, Granted: true}
		if err := check(v); err != nil {
			g.Granted, g.Reason = false, err.Error()
		}
		out = append(out, g)
	}
	return out
}

// ScopeGrants implements tools.ScopedTool. Memory reads one `scope` argument
// two ways: the key/value, fact and note operations check memory_scopes, and
// the sql_* operations check sql_scopes. Both are reported.
func (m *Memory) ScopeGrants(ctx context.Context) []tools.ScopeField {
	schema := m.InputSchema()
	return []tools.ScopeField{
		{Grants: scopeGrants(schema, func(s string) error {
			_, _, err := m.resolveScope(ctx, s)
			return err
		})},
		{Applies: "sql_* ops", Grants: scopeGrants(schema, func(s string) error {
			_, _, err := m.resolveSqlScope(ctx, s)
			return err
		})},
	}
}

// ScopeGrants implements tools.ScopedTool.
func (d *Document) ScopeGrants(ctx context.Context) []tools.ScopeField {
	return []tools.ScopeField{{Grants: scopeGrants(d.InputSchema(), func(s string) error {
		_, _, err := d.resolveScope(ctx, s)
		return err
	})}}
}

// ScopeGrants implements tools.ScopedTool.
func (p *Path) ScopeGrants(ctx context.Context) []tools.ScopeField {
	return []tools.ScopeField{{Grants: scopeGrants(p.InputSchema(), func(s string) error {
		_, _, _, err := p.resolveScope(ctx, s)
		return err
	})}}
}

// ScopeGrants implements tools.ScopedTool.
func (h *History) ScopeGrants(ctx context.Context) []tools.ScopeField {
	return []tools.ScopeField{{Grants: scopeGrants(h.InputSchema(), func(s string) error {
		_, err := h.authorizedScope(ctx, s)
		return err
	})}}
}

// ScopeGrants implements tools.ScopedTool.
func (c *CredentialDef) ScopeGrants(ctx context.Context) []tools.ScopeField {
	return []tools.ScopeField{{Grants: scopeGrants(c.InputSchema(), func(s string) error {
		_, _, _, err := c.resolveScope(ctx, s)
		return err
	})}}
}

var (
	_ tools.ScopedTool = (*Memory)(nil)
	_ tools.ScopedTool = (*Document)(nil)
	_ tools.ScopedTool = (*Path)(nil)
	_ tools.ScopedTool = (*History)(nil)
	_ tools.ScopedTool = (*CredentialDef)(nil)
	_ tools.HelpIndex  = (*Context)(nil)
)
