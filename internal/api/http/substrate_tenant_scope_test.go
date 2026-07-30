package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestSubstratePlane_GrantsTenantScopeLikeTheMCPPlane pins the two operator planes
// to the same memory reach.
//
// A tenant document needs BOTH grants — structure lives in SQL Memory, chunk bodies
// in Memory — and the MCP plane got them when tenant scope shipped while this HTTP
// plane was missed. The result was a split-brain operator surface: the same tenant
// document was reachable through an MCP session and refused through the Web UI,
// which reads the HTTP plane. Worse than either answer, because the UI could list
// the document in the Path tree and then fail to open it.
//
// `global` is intentionally absent from the SQL half: SQL Memory has no global tier,
// so advertising one would trade a clear refusal for a confusing provisioning error.
func TestSubstratePlane_GrantsTenantScopeLikeTheMCPPlane(t *testing.T) {
	ctx := substrateAdminCtx(context.Background())

	mem := tools.MemoryPolicy(ctx).AllowedScopes
	for _, want := range []string{"agent", "user", "tenant", "global"} {
		if !containsStr(mem, want) {
			t.Errorf("memory scope %q missing from the operator plane: %v", want, mem)
		}
	}
	sql := tools.SqlMemPolicy(ctx).AllowedScopes
	for _, want := range []string{"agent", "user", "tenant"} {
		if !containsStr(sql, want) {
			t.Errorf("sql scope %q missing from the operator plane: %v", want, sql)
		}
	}
	if containsStr(sql, "global") {
		t.Errorf("SQL Memory has no global tier; advertising one misleads: %v", sql)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
