package builtin

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Off a run, a lease is held by the calling principal: two members never
// share an owner (so neither releases or extends the other's lease), and
// neither is labelled the operator — whatever agent name, synthetic or none,
// the transport stamped. A run's lease is still its run's.
func TestDocumentLeaseOwner_EachOffRunPrincipalOwnsItsOwn(t *testing.T) {
	for _, agent := range []string{"", "http-admin", "grpc-admin", "mcp-operator"} {
		owner := func(subject string) string {
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: subject, Scopes: []string{auth.ScopeRunsCreate}})
			// Every off-run plane stamps the caller's tier; a member's here.
			ctx = tools.WithCallerTier(ctx, tools.TierMember)
			return documentLeaseOwner(tools.WithAgentName(ctx, agent))
		}
		bob, carol := owner("bob"), owner("carol")
		if bob != "principal:bob" || carol != "principal:carol" {
			t.Errorf("agent %q: owners %q and %q, want principal:bob and principal:carol", agent, bob, carol)
		}
	}
	run := tools.WithRunID(auth.WithPrincipal(context.Background(), auth.Principal{Subject: "bob"}), "run-1")
	if got := documentLeaseOwner(run); got != "run-1" {
		t.Errorf("a run's lease owner = %q, want its run id", got)
	}
}
