package aclres

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// A workers:attach credential's baseline denies every question the catalog
// can ask, so no list an operator binds to the worker's subject can open an
// API surface to it: an explicit deny beats any allow.
func TestResolve_WorkerAttacherBaselineDeniesEveryType(t *testing.T) {
	r := New(newStore(t), authz.ModeEnforce, 0)
	p, err := r.Resolve(context.Background(), auth.Principal{TenantID: "acme", Subject: "worker-1", TokenDefID: "def_w", Scopes: []string{auth.ScopeWorkersAttach}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective == nil {
		t.Fatal("no compiled set")
	}
	for _, q := range []struct{ action, typ, name string }{
		{"read", "runtime", "metrics"}, {"run", "agent", "chat"}, {"read", "memory", "tenant"},
		{"mint", "principal", "token:self"}, {"invoke", "mcp-tool", "context"}, {"use", "gateway", "chat"},
	} {
		if got := decide(p.Effective, q.action, q.typ, q.name); got != authz.Denied {
			t.Errorf("%s %s:%s = %v, want denied", q.action, q.typ, q.name, got)
		}
	}
}
