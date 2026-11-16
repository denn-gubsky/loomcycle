package erasure

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// The render log (RFC GR §4.8) records who rendered a document: a run, or a
// person who downloaded it (the off-run export). A user's own scope goes
// whole with the user; what is left are rows in the tenant's shared and
// agent scopes naming the subject. Those rows are kept — they say a file was
// made, with hashes and sizes, never content — and the subject's name is
// cleared from them.

// renderActorPlane is the plane key in the report and the result.
const renderActorPlane = "document_render_actors"

// renderActorRows counts (clear false) or clears (clear true) the render log
// rows of the tenant's tenant and agent scopes whose actor is subject. A
// scope whose document plane was never provisioned, or that predates the
// render log, has none; nothing here provisions one.
func (s *Service) renderActorRows(ctx context.Context, tenant, subject string, clear bool) (int, error) {
	if s.SqlMem == nil {
		return 0, nil
	}
	dp := s.SqlMem.DocPlane()
	scopes, err := s.SqlMem.ListScopes(ctx)
	if err != nil {
		return 0, err
	}
	want := sqlScopeTenant(tenant)
	total := 0
	for _, sk := range scopes {
		if sk.Tenant != want || (sk.Scope != "tenant" && sk.Scope != "agent") {
			continue
		}
		key := sqlmem.ScopeKey{Tenant: sk.Tenant, Scope: sk.Scope, ScopeID: sk.ScopeID}
		if ok, err := documentsMayExist(ctx, s.SqlMem, key); err != nil {
			return total, err
		} else if !ok {
			continue
		}
		if clear {
			res, err := dp.Exec(ctx, key, dp.Rebind(`UPDATE document_renders SET actor = '' WHERE actor = ?`), []any{subject}, 0)
			if tableMissing(err) {
				continue
			}
			if err != nil {
				return total, err
			}
			total += int(res.RowsAffected)
			continue
		}
		res, err := dp.Query(ctx, key, dp.Rebind(`SELECT COUNT(*) FROM document_renders WHERE actor = ?`), []any{subject})
		if tableMissing(err) {
			continue
		}
		if err != nil {
			return total, err
		}
		if len(res.Rows) == 1 && len(res.Rows[0]) == 1 {
			total += asCount(res.Rows[0][0])
		}
	}
	return total, nil
}

// tableMissing is a scope whose render log table does not exist yet.
func tableMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") || strings.Contains(msg, "42p01") ||
		(strings.Contains(msg, "document_renders") && strings.Contains(msg, "does not exist"))
}

func asCount(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case int32:
		return int(n)
	case float64:
		return int(n)
	case string:
		c := 0
		for _, r := range n {
			if r < '0' || r > '9' {
				return 0
			}
			c = c*10 + int(r-'0')
		}
		return c
	}
	return 0
}

// documentsMayExist reports whether a scope can hold document tables without
// a read provisioning them: its document plane exists, or (sqlite) the scope
// exists and its documents have not moved to a file of their own yet, so
// they are read from the scope's file.
func documentsMayExist(ctx context.Context, m *sqlmem.Manager, key sqlmem.ScopeKey) (bool, error) {
	ok, err := m.DocPlane().PlaneExists(ctx, key)
	if err != nil || ok {
		return ok, err
	}
	if m.Tier() != "sqlite" {
		return false, nil
	}
	return m.ScopeExists(ctx, key)
}
